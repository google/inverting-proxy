/*
Copyright 2018 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	hellopb "google.golang.org/grpc/examples/helloworld/helloworld"
)

const (
	backendCookie = "backend-cookie"
	sessionCookie = "proxy-sessions-cookie"
)

func checkRequest(proxyURL, testPath, want string, timeout time.Duration, expectedCookie string) error {
	jarOptions := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	jar, err := cookiejar.New(&jarOptions)
	if err != nil {
		return fmt.Errorf("Failure creating a cookie jar: %v", err)
	}
	client := &http.Client{
		Timeout: timeout,
		Jar:     jar,
	}
	reqURL := proxyURL + testPath
	parsedReqURL, err := url.Parse(reqURL)
	if err != nil {
		return fmt.Errorf("internal error parsing a test URL: %v", err)
	}
	resp, err := client.Get(reqURL)
	if err != nil {
		return fmt.Errorf("failed to issue a frontend GET request: %v", err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read the response body: %v", err)
	}
	if got := string(body); got != want {
		return fmt.Errorf("unexpected proxy frontend response; got %q, want %q", got, want)
	}

	cookies := jar.Cookies(parsedReqURL)
	if len(cookies) != 1 {
		return fmt.Errorf("unexpected number of cookies set: %v", cookies)
	} else if got, want := cookies[0].Name, expectedCookie; got != want {
		return fmt.Errorf("unexpected response cookie set: got %q, want %q", got, want)
	}
	return nil
}

func RunLocalProxy(ctx context.Context, t *testing.T) (int, error) {
	// This assumes that "Make build" has been run
	proxyArgs := strings.Join(append(
		[]string{"${GOPATH}/bin/inverting-proxy"},
		"--port=0"),
		" ")
	proxyCmd := exec.CommandContext(ctx, "/bin/bash", "-c", proxyArgs)

	var proxyOut bytes.Buffer
	proxyCmd.Stdout = &proxyOut
	proxyCmd.Stderr = &proxyOut
	if err := proxyCmd.Start(); err != nil {
		t.Fatalf("Failed to start the inverting-proxy binary: %v", err)
	}
	go func() {
		err := proxyCmd.Wait()
		t.Logf("Proxy result: %v, stdout/stderr: %q", err, proxyOut.String())
	}()
	for i := 0; i < 30; i++ {
		for _, line := range strings.Split(proxyOut.String(), "\n") {
			if strings.Contains(line, "Listening on [::]:") {
				portStr := strings.TrimSpace(strings.Split(line, "Listening on [::]:")[1])
				return strconv.Atoi(portStr)
			}
		}
		t.Logf("Waiting for the locally running proxy to start...")
		time.Sleep(1 * time.Second)
	}
	return 0, fmt.Errorf("Locally-running proxy failed to start up in time: %q", proxyOut.String())
}

func RunBackend(ctx context.Context, t *testing.T) string {
	backendCookieVal := uuid.New().String()
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bc, err := r.Cookie(backendCookie)
		if err == http.ErrNoCookie || bc == nil {
			bc = &http.Cookie{
				Name:     backendCookie,
				Value:    backendCookieVal,
				HttpOnly: true,
			}
			http.SetCookie(w, bc)
			http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
			return
		}
		if got, want := bc.Value, backendCookieVal; got != want {
			t.Errorf("Unexepected backend cookie value: got %q, want %q", got, want)
		}
		w.Write([]byte(r.URL.Path))
	}))
	go func() {
		<-ctx.Done()
		backendServer.Close()
	}()
	return backendServer.URL
}

type helloRPCServer struct {
	hellopb.UnimplementedGreeterServer
}

func (h *helloRPCServer) SayHello(ctx context.Context, req *hellopb.HelloRequest) (*hellopb.HelloReply, error) {
	return &hellopb.HelloReply{
		Message: req.GetName(),
	}, nil
}

func RunGRPCBackend(ctx context.Context, t *testing.T) (string, error) {
	grpcServer := grpc.NewServer()
	hellopb.RegisterGreeterServer(grpcServer, &helloRPCServer{})
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		return "", fmt.Errorf("failure listening on a TCP port: %w", err)
	}
	go grpcServer.Serve(lis)
	go func() {
		<-ctx.Done()
		grpcServer.Stop()
		lis.Close()
	}()
	return lis.Addr().String(), nil
}

func RunFakeMetadataServer(ctx context.Context, t *testing.T) string {
	fakeMetadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Emulate slow responses from the metadata server, to check that the agent
		// is appropriately caching the results.
		time.Sleep(50 * time.Millisecond)
		if strings.HasPrefix(r.URL.Path, "/computeMetadata/v1/project/project-id") {
			io.WriteString(w, "12345")
			return
		}
		if !(strings.HasPrefix(r.URL.Path, "/computeMetadata/v1/instance/service-accounts/") && strings.HasSuffix(r.URL.Path, "/token")) {
			io.WriteString(w, "ok")
			return
		}
		var fakeToken struct {
			AccessToken  string `json:"access_token"`
			ExpiresInSec int    `json:"expires_in"`
			TokenType    string `json:"token_type"`
		}
		fakeToken.AccessToken = "fakeToken"
		fakeToken.ExpiresInSec = 1000
		fakeToken.TokenType = "Bearer"
		if err := json.NewEncoder(w).Encode(&fakeToken); err != nil {
			t.Logf("Failed to encode a fake service account credential: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	go func() {
		<-ctx.Done()
		fakeMetadata.Close()
	}()
	return fakeMetadata.URL
}

func TestWithInMemoryProxyAndBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendHomeDir, err := ioutil.TempDir("", "backend-home")
	if err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	gcloudCfg := filepath.Join(backendHomeDir, ".config", "gcloud")
	if err := os.MkdirAll(gcloudCfg, os.ModePerm); err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	backendURL := RunBackend(ctx, t)
	fakeMetadataURL := RunFakeMetadataServer(ctx, t)

	parsedBackendURL, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("Failed to parse the backend URL: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx, t)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		t.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	t.Logf("Started backend at localhost:%s and proxy at %s", parsedBackendURL.Port(), proxyURL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--backend=testBackend",
		"--proxy", proxyURL+"/",
		"--host=localhost:"+parsedBackendURL.Port()),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)

	var out bytes.Buffer
	agentCmd.Stdout = &out
	agentCmd.Stderr = &out
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadataURL, "http://"))
	if err := agentCmd.Start(); err != nil {
		t.Fatalf("Failed to start the agent binary: %v", err)
	}
	defer func() {
		cancel()
		err := agentCmd.Wait()
		t.Logf("Agent result: %v, stdout/stderr: %q", err, out.String())
	}()

	// Send one request through the proxy to make sure the agent has come up.
	//
	// We give this initial request a long time to complete, as the agent takes
	// a long time to start up.
	testPath := "/some/request/path"
	if err := checkRequest(proxyURL, testPath, testPath, time.Second, backendCookie); err != nil {
		t.Fatalf("Failed to send the initial request: %v", err)
	}

	for i := 0; i < 10; i++ {
		// The timeout below was chosen to be overly generous to prevent test flakiness.
		//
		// This has the consequence that it will only catch severe latency regressions.
		//
		// The specific value was chosen by running the test in a loop 100 times and
		// incrementing the value until all 100 runs passed.
		if err := checkRequest(proxyURL, testPath, testPath, 100*time.Millisecond, backendCookie); err != nil {
			t.Fatalf("Failed to send request %d: %v", i, err)
		}
	}
}

func TestWithInMemoryProxyAndBackendWithSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendHomeDir, err := ioutil.TempDir("", "backend-home")
	if err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	gcloudCfg := filepath.Join(backendHomeDir, ".config", "gcloud")
	if err := os.MkdirAll(gcloudCfg, os.ModePerm); err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	backendURL := RunBackend(ctx, t)
	fakeMetadataURL := RunFakeMetadataServer(ctx, t)

	parsedBackendURL, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("Failed to parse the backend URL: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx, t)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		t.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	t.Logf("Started backend at localhost:%s and proxy at %s", parsedBackendURL.Port(), proxyURL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--backend=testBackend",
		"--proxy", proxyURL+"/",
		"--session-cookie-name="+sessionCookie,
		"--disable-ssl-for-test=true",
		"--inject-banner=\\<div\\>FOO\\</div\\>",
		"--favicon-url=www.favicon.com/test.png",
		"--host=localhost:"+parsedBackendURL.Port()),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)

	var out bytes.Buffer
	agentCmd.Stdout = &out
	agentCmd.Stderr = &out
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadataURL, "http://"))
	if err := agentCmd.Start(); err != nil {
		t.Fatalf("Failed to start the agent binary: %v", err)
	}
	defer func() {
		cancel()
		err := agentCmd.Wait()
		t.Logf("Agent result: %v, stdout/stderr: %q", err, out.String())
	}()

	// Send one request through the proxy to make sure the agent has come up.
	//
	// We give this initial request a long time to complete, as the agent takes
	// a long time to start up.
	testPath := "/some/request/path"
	if err := checkRequest(proxyURL, testPath, testPath, time.Second, sessionCookie); err != nil {
		t.Fatalf("Failed to send the initial request: %v", err)
	}

	for i := 0; i < 10; i++ {
		// The timeout below was chosen to be overly generous to prevent test flakiness.
		//
		// This has the consequence that it will only catch severe latency regressions.
		//
		// The specific value was chosen by running the test in a loop 100 times and
		// incrementing the value until all 100 runs passed.
		if err := checkRequest(proxyURL, testPath, testPath, 100*time.Millisecond, sessionCookie); err != nil {
			t.Fatalf("Failed to send request %d: %v", i, err)
		}
	}
}

func TestProxyTimeoutWithShortTimeout(t *testing.T) {
	proxyTimeout := "10ms"
	requestForwardingTimeout := "60s"
	wantTimeout := true

	timeoutTest(t, proxyTimeout, requestForwardingTimeout, wantTimeout)
}

func TestProxyTimeoutWithLongTimeout(t *testing.T) {
	proxyTimeout := "60s"
	requestForwardingTimeout := "60s"
	wantTimeout := false

	timeoutTest(t, proxyTimeout, requestForwardingTimeout, wantTimeout)
}

func timeoutTest(t *testing.T, proxyTimeout string, requestForwardingTimeout string, wantTimeout bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendHomeDir := filepath.Join(t.TempDir(), "backend-home")
	gcloudCfg := filepath.Join(backendHomeDir, ".config", "gcloud")
	if err := os.MkdirAll(gcloudCfg, os.ModePerm); err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	backendURL := RunBackend(ctx, t)
	fakeMetadataURL := RunFakeMetadataServer(ctx, t)

	parsedBackendURL, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("Failed to parse the backend URL: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx, t)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		t.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	t.Logf("Started backend at localhost:%s and proxy at %s", parsedBackendURL.Port(), proxyURL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--backend=testBackend",
		"--proxy", proxyURL+"/",
		"--proxy-timeout="+proxyTimeout,
		"--request-forwarding-timeout="+requestForwardingTimeout,
		"--host=localhost:"+parsedBackendURL.Port()),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)

	var out bytes.Buffer
	agentCmd.Stdout = &out
	agentCmd.Stderr = &out
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadataURL, "http://"))
	if err := agentCmd.Start(); err != nil {
		t.Fatalf("Failed to start the agent binary: %v", err)
	}
	defer func() {
		cancel()
		err := agentCmd.Wait()

		s := out.String()
		t.Logf("Agent result: %v, stdout/stderr: %q", err, s)
		timeoutOccurred := strings.Contains(s, "context deadline exceeded")
		if timeoutOccurred != wantTimeout {
			t.Errorf("Unexpected timeout state: got %v, want %v", timeoutOccurred, wantTimeout)
		}
	}()

	// Send one request through the proxy to make sure the agent has come up.
	//
	// We give this initial request a long time to complete, as the agent takes
	// a long time to start up.
	testPath := "/some/request/path"
	if err := checkRequest(proxyURL, testPath, testPath, time.Second, backendCookie); err != nil {
		t.Fatalf("Failed to send the initial request: %v", err)
	}

	if err := checkRequest(proxyURL, testPath, testPath, 100*time.Millisecond, backendCookie); err != nil {
		t.Fatalf("Failed to send request %v", err)
	}
}

func TestGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendHomeDir, err := ioutil.TempDir("", "backend-home")
	if err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	gcloudCfg := filepath.Join(backendHomeDir, ".config", "gcloud")
	if err := os.MkdirAll(gcloudCfg, os.ModePerm); err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	backendURL := RunBackend(ctx, t)
	fakeMetadataURL := RunFakeMetadataServer(ctx, t)

	parsedBackendURL, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("Failed to parse the backend URL: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx, t)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		t.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	t.Logf("Started backend at localhost:%s and proxy at %s", parsedBackendURL.Port(), proxyURL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--graceful-shutdown-timeout=1s",
		"--backend=testBackend",
		"--proxy", proxyURL+"/",
		"--host=localhost:"+parsedBackendURL.Port()),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)

	var out bytes.Buffer
	agentCmd.Stdout = &out
	agentCmd.Stderr = &out
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadataURL, "http://"))
	if err := agentCmd.Start(); err != nil {
		t.Fatalf("Failed to start the agent binary: %v", err)
	}
	defer func() {
		cancel()
		err := agentCmd.Wait()
		t.Logf("Agent result: %v, stdout/stderr: %q", err, out.String())
	}()

	// Send one request through the proxy to make sure the agent has come up.
	//
	// We give this initial request a long time to complete, as the agent takes
	// a long time to start up.
	testPath := "/some/request/path"
	if err := checkRequest(proxyURL, testPath, testPath, time.Second, backendCookie); err != nil {
		t.Fatalf("Failed to send the initial request: %v", err)
	}

	for i := 0; i < 10; i++ {
		// The timeout below was chosen to be overly generous to prevent test flakiness.
		//
		// This has the consequence that it will only catch severe latency regressions.
		//
		// The specific value was chosen by running the test in a loop 100 times and
		// incrementing the value until all 100 runs passed.
		if err := checkRequest(proxyURL, testPath, testPath, 100*time.Millisecond, backendCookie); err != nil {
			t.Fatalf("Failed to send request %d: %v", i, err)
		}
	}

	agentCmd.Process.Signal(syscall.SIGINT)
	waitCh := make(chan struct{})
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		agentCmd.Wait()
		close(waitCh)
	}()

	for {
		select {
		case <-waitCh:
			return
		case <-waitCtx.Done():
			t.Fatal("Timed out waiting for the agent to exit.")
			return
		}
	}
}

func TestHTTP2Backend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	backendHomeDir, err := ioutil.TempDir("", "backend-home")
	if err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	gcloudCfg := filepath.Join(backendHomeDir, ".config", "gcloud")
	if err := os.MkdirAll(gcloudCfg, os.ModePerm); err != nil {
		t.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	fakeMetadataURL := RunFakeMetadataServer(ctx, t)
	backendHost, err := RunGRPCBackend(ctx, t)
	if err != nil {
		t.Fatalf("Failure running a gRPC backend: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx, t)
	if err != nil {
		t.Fatalf("Failure running the local proxy: %v", err)
	}
	proxyURL := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("localhost:%d", proxyPort),
	}
	t.Logf("Started backend at %s and proxy at %s", backendHost, proxyURL.String())

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--backend=testBackend",
		"--proxy", proxyURL.String()+"/",
		"--disable-ssl-for-test=true",
		"--force-http2",
		"--host="+backendHost),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)

	var out bytes.Buffer
	agentCmd.Stdout = &out
	agentCmd.Stderr = &out
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadataURL, "http://"))
	if err := agentCmd.Start(); err != nil {
		t.Fatalf("Failed to start the agent binary: %v", err)
	}
	defer func() {
		cancel()
		err := agentCmd.Wait()
		t.Logf("Agent result: %v, stdout/stderr: %q", err, out.String())
	}()

	// Wrap the in-memory proxy with a test server that supports TLS
	tlsProxy := httputil.NewSingleHostReverseProxy(proxyURL)
	tlsProxyServer := httptest.NewUnstartedServer(tlsProxy)
	tlsProxyServer.EnableHTTP2 = true
	tlsProxyServer.StartTLS()
	defer tlsProxyServer.Close()
	certPool := x509.NewCertPool()
	certPool.AddCert(tlsProxyServer.Certificate())
	clientTransportCredentials := credentials.NewTLS(&tls.Config{
		RootCAs: certPool,
	})
	conn, err := grpc.Dial(strings.TrimPrefix(tlsProxyServer.URL, "https://"), grpc.WithTransportCredentials(clientTransportCredentials))
	if err != nil {
		t.Fatalf("Failed to dial the gRPC connection: %v", err)
	}
	defer conn.Close()
	helloClient := hellopb.NewGreeterClient(conn)
	name := "Example Name"
	resp, err := helloClient.SayHello(ctx, &hellopb.HelloRequest{Name: name})
	if err != nil {
		t.Errorf("helloClient.SayHello: %v", err)
	} else if got, want := resp.Message, name; got != want {
		t.Errorf("Unexpected response from SayHello: got %q, want %q", got, want)
	}
}

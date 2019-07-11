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
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"context"
)

func checkRequest(proxyURL, testPath, want string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
	}
	resp, err := client.Get(proxyURL + testPath)
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
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Responding to backend request to %q", r.URL.Path)
		w.Write([]byte(r.URL.Path))
	}))
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
		backend.Close()
		fakeMetadata.Close()
	}()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("Failed to parse the backend URL: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx, t)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		t.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	t.Logf("Started backend at localhost:%s and proxy at %s", backendURL.Port(), proxyURL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--backend=testBackend",
		"--proxy", proxyURL+"/",
		"--host=localhost:"+backendURL.Port()),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)

	var out bytes.Buffer
	agentCmd.Stdout = &out
	agentCmd.Stderr = &out
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadata.URL, "http://"))
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
	if err := checkRequest(proxyURL, testPath, testPath, time.Second); err != nil {
		t.Fatalf("Failed to send the initial request: %v", err)
	}

	for i := 0; i < 10; i++ {
		// The timeout below was chosen to be overly generous to prevent test flakiness.
		//
		// This has the consequence that it will only catch severe latency regressions.
		//
		// The specific value was chosen by running the test in a loop 100 times and
		// incrementing the value until all 100 runs passed.
		if err := checkRequest(proxyURL, testPath, testPath, 100*time.Millisecond); err != nil {
			t.Fatalf("Failed to send request %d: %v", i, err)
		}
	}
}

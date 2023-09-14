/*
Copyright 2023 Google Inc. All rights reserved.

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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/google/inverting-proxy/agent/utils"
)

type nopWriteCloser struct {
	wrapped io.Writer
}

func (n *nopWriteCloser) Write(b []byte) (int, error) {
	return n.wrapped.Write(b)
}

func (n *nopWriteCloser) Close() error {
	return nil
}

type responseForwarder struct {
	proxyHandler       http.Handler
	endpointID         string
	requestID          string
	r                  *http.Request
	header             http.Header
	responseBodyWriter io.WriteCloser
}

func (r *responseForwarder) Header() http.Header {
	return r.header
}

func (r *responseForwarder) WriteHeader(statusCode int) {
	if r.responseBodyWriter != nil {
		return
	}

	proxyRequestBodyReader, proxyRequestBodyWriter := io.Pipe()
	proxyReq := httptest.NewRequest(http.MethodPost, "/", proxyRequestBodyReader)
	proxyReq.Header.Set(utils.HeaderBackendID, r.endpointID)
	proxyReq.Header.Set(utils.HeaderRequestID, r.requestID)
	proxyReq.Header.Set("Content-Type", "text/plain")
	go func(proxyReq *http.Request) {
		rr := httptest.NewRecorder()
		r.proxyHandler.ServeHTTP(rr, proxyReq)
		proxyResp := rr.Result()
		defer proxyResp.Body.Close()
		respBody, err := io.ReadAll(proxyResp.Body)
		if err != nil {
			log.Printf("Failure reading a proxy response: %v", err)
		}
		if proxyResp.StatusCode != http.StatusOK {
			log.Printf("Unexpected proxy response: %q, %s", proxyResp.Status, string(respBody))
		}
	}(proxyReq)

	backendResponseBodyReader, backendResponseBodyWriter := io.Pipe()
	r.responseBodyWriter = backendResponseBodyWriter
	response := &http.Response{
		StatusCode:       statusCode,
		Status:           http.StatusText(statusCode),
		TransferEncoding: []string{"chunked"},
		Proto:            r.r.Proto,
		ProtoMajor:       r.r.ProtoMajor,
		ProtoMinor:       r.r.ProtoMinor,
		Header:           make(http.Header),
		Body:             backendResponseBodyReader,
		Trailer:          make(http.Header),
	}
	for k, v := range r.header {
		response.Header[k] = v
	}
	go func(response *http.Response, writer io.WriteCloser) {
		defer writer.Close()
		if err := response.Write(writer); err != nil {
			log.Printf("Failure writing a backend response to the proxy: %v", err)
		}
	}(response, proxyRequestBodyWriter)
}

func (r *responseForwarder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.responseBodyWriter.Write(b)
}

func (r *responseForwarder) Close() error {
	r.WriteHeader(http.StatusOK)
	return r.responseBodyWriter.Close()
}

type requestForwarder struct {
	proxyHandler      http.Handler
	endpointID        string
	requestID         string
	backend           http.Handler
	header            http.Header
	requestBodyWriter io.WriteCloser
}

func (r *requestForwarder) Header() http.Header {
	return r.header
}

func (r *requestForwarder) WriteHeader(statusCode int) {
	if r.requestBodyWriter != nil {
		return
	}
	if statusCode != http.StatusOK {
		log.Printf("Unexpected proxy response code when reading a pending request: %d, %q", statusCode, http.StatusText(statusCode))
		r.requestBodyWriter = &nopWriteCloser{log.Default().Writer()}
		return
	}
	requestBodyReader, requestBodyWriter := io.Pipe()
	r.requestBodyWriter = requestBodyWriter
	go func(bodyReader io.ReadCloser) {
		defer bodyReader.Close()
		req, err := http.ReadRequest(bufio.NewReader(bodyReader))
		if err != nil {
			log.Printf("Error reading a proxied request: %v", err)
			return
		}
		w := &responseForwarder{
			proxyHandler: r.proxyHandler,
			endpointID:   r.endpointID,
			requestID:    r.requestID,
			r:            req,
		}
		defer w.Close()
		r.backend.ServeHTTP(w, req)
	}(requestBodyReader)
}

func (r *requestForwarder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.requestBodyWriter.Write(b)
}

func (r *requestForwarder) Close() error {
	r.WriteHeader(http.StatusOK)
	return r.requestBodyWriter.Close()
}

func runEndpoint(ctx context.Context, proxyURL, endpointID string, backend http.Handler) error {
	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		return err
	}
	h := httputil.NewSingleHostReverseProxy(parsedURL)
	h.Transport = &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		listReq := httptest.NewRequest(http.MethodGet, "/", nil)
		listReq = listReq.WithContext(ctx)
		listReq.Header.Set(utils.HeaderBackendID, endpointID)
		listRR := httptest.NewRecorder()
		h.ServeHTTP(listRR, listReq)
		listResp := listRR.Result()
		defer listResp.Body.Close()
		if listResp.StatusCode != http.StatusOK {
			continue
		}
		listRespBody, err := io.ReadAll(listResp.Body)
		if err != nil {
			return err
		}
		var pending []string
		if err := json.Unmarshal(listRespBody, &pending); err != nil {
			return err
		}
		if len(pending) > 0 {
			log.Printf("Got %d pending requests: %+v", len(pending), pending)
		}
		for _, requestID := range pending {
			go func(requestID string) {
				getReq := httptest.NewRequest(http.MethodGet, "/", nil)
				getReq = getReq.WithContext(ctx)
				getReq.Header.Set(utils.HeaderBackendID, endpointID)
				getReq.Header.Set(utils.HeaderRequestID, requestID)
				getRespWriter := &requestForwarder{
					proxyHandler: h,
					endpointID:   endpointID,
					requestID:    requestID,
					backend:      backend,
					header:       make(http.Header),
				}
				log.Printf("Requesting body for %q...", requestID)
				h.ServeHTTP(getRespWriter, getReq)
			}(requestID)
		}
	}
}

func RunLocalProxy(ctx context.Context, t *testing.T, requestTimeout time.Duration) (int, error) {
	// This assumes that "Make build" has been run
	proxyArgs := append(
		[]string{"${GOPATH}/bin/inverting-proxy"},
		"--port=0")
	if requestTimeout > 0 {
		proxyArgs = append(proxyArgs,
			fmt.Sprintf("--request-timeout=%s", requestTimeout),
			"--request-queue-size=10",
			"--list-pending-timeout=10ms")
	}
	proxyCmd := exec.CommandContext(ctx, "/bin/bash", "-c", strings.Join(proxyArgs, " "))

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

type infiniteReader struct {
}

func (d *infiniteReader) Read(b []byte) (n int, err error) {
	for i := 0; i < len(b); i++ {
		b[i] = byte(97)
	}
	return len(b), nil
}

func TestWithInMemoryProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxyPort, err := RunLocalProxy(ctx, t, time.Second)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		t.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	t.Logf("Started in-memory proxy at %s", proxyURL)

	log.Printf("Starting backend...")
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Processing request: %+v", r)
		if r.Method == http.MethodGet {
			w.Write([]byte(r.Method + ": " + r.URL.Path))
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			default:
				if _, err := io.CopyN(w, r.Body, 128); err != nil {
					return
				}
			}
		}
	})
	endpointID := "test-endpoint"
	go func() {
		if err := runEndpoint(ctx, proxyURL, endpointID, backend); err != nil {
			log.Printf("Failure while running a proxy endpoint backend: %v", err)
		}
	}()

	client := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}
	resp, err := client.Get(proxyURL)
	if err != nil {
		t.Fatalf("failed to issue a frontend GET request: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failure reading the response body: %v", err)
	}
	if got, want := string(bodyBytes), "GET: /"; got != want {
		t.Errorf("Unexpected response body: got %q, want %q", got, want)
	}

	resp2, err := client.Post(proxyURL, "text/plain", strings.NewReader("abcde"))
	if err != nil {
		t.Fatalf("failed to issue a frontend POST request: %v", err)
	}
	defer resp2.Body.Close()
	body2Bytes, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("Failure reading the second response body: %v", err)
	}
	if got, want := string(body2Bytes), "abcde"; got != want {
		t.Errorf("Unexpected second response body: got %q, want %q", got, want)
	}

	resp3, err := client.Post(proxyURL, "text/plain", io.NopCloser(&infiniteReader{}))
	if err != nil {
		t.Fatalf("failed to issue a frontend POST request: %v", err)
	}
	defer resp3.Body.Close()
	body3Bytes, err := io.ReadAll(&io.LimitedReader{
		R: resp3.Body,
		N: 32,
	})
	if err != nil {
		t.Fatalf("Failure reading the second response body: %v", err)
	}
	if got, want := string(body3Bytes), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; got != want {
		t.Errorf("Unexpected second response body: got %q, want %q", got, want)
	}
}

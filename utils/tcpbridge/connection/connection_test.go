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

package connection

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strconv"
	"strings"
	"testing"
)

func testHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	io.Copy(w, r.Body)
}

func TestBridgedNoopConnection(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(testHandler))
	defer testServer.Close()

	testServerURL, err := url.Parse(testServer.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	testServerPort, err := strconv.Atoi(testServerURL.Port())
	if err != nil {
		t.Fatalf("strconv.Atoi: %v", err)
	}

	bh := Handler(testServerPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "TEST FAILURE", http.StatusBadGateway)
	}))
	backendServer := httptest.NewServer(bh)
	defer backendServer.Close()

	backendServerURL, err := url.Parse(backendServer.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	backendServerURL.Scheme = "ws"
	backendServerURL.Path = path.Join(backendServerURL.Path, StreamingPath)

	transport := &http.Transport{
		Dial: func(_, _ string) (net.Conn, error) {
			return DialWebsocket(context.Background(), backendServerURL, nil)
		},
	}
	testMessage := "Hello"
	req, err := http.NewRequest("POST", "http://localhost:8080/", strings.NewReader(testMessage))
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Errorf("transport.RoundTrip: %v", err)
	} else if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("transport.RoundTrip: unexpected status. got %d, want %d", resp.StatusCode, http.StatusOK)
	} else {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("io.ReadAll: %v", err)
		} else if got, want := string(respBody), testMessage; got != want {
			t.Errorf("transport.RoundTrip: unexpected response body. got %s, want %s", got, want)
		}
	}
}

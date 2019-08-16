/*
Copyright 2017 Google Inc. All rights reserved.

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

package utils

import (
	"bufio"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseRequests(t *testing.T) {
	mockEmptyPendingRequestsResponse := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(strings.NewReader("")),
	}
	emptyPendingRequests, err := parseRequestIDs(mockEmptyPendingRequestsResponse)
	if err != nil {
		t.Fatal(err)
	}
	if emptyPendingRequests == nil || len(emptyPendingRequests) > 0 {
		t.Fatal("Unexpected response for empty pending requests")
	}

	mockNoPendingRequestsResponse := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: 2,
		Body:          ioutil.NopCloser(strings.NewReader("[]")),
	}
	noPendingRequests, err := parseRequestIDs(mockNoPendingRequestsResponse)
	if err != nil {
		t.Fatal(err)
	}
	if noPendingRequests == nil || len(noPendingRequests) > 0 {
		t.Fatal("Unexpected response for no pending requests")
	}

	mockFailedPendingRequestsResponse := &http.Response{
		StatusCode:    http.StatusInternalServerError,
		ContentLength: 6,
		Body:          ioutil.NopCloser(strings.NewReader("whoops")),
	}
	failedPendingRequests, err := parseRequestIDs(mockFailedPendingRequestsResponse)
	if failedPendingRequests != nil {
		t.Fatal("Unexpected response for failed pending requests response")
	}
	if !strings.Contains(err.Error(), "whoops") {
		t.Fatal("Failed to cascade error message")
	}

	mockNormalPendingRequestsResponse := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: 10,
		Body:          ioutil.NopCloser(strings.NewReader("[\"A\", \"B\"]")),
	}
	normalPendingRequests, err := parseRequestIDs(mockNormalPendingRequestsResponse)
	if err != nil {
		t.Fatal(err)
	}
	if normalPendingRequests[0] != "A" || normalPendingRequests[1] != "B" {
		t.Fatal("Unexpected response for normal pending requests")
	}
}

func TestParseRequestFromProxyResponse(t *testing.T) {
	mockFailedProxyResponse := &http.Response{
		StatusCode:    http.StatusInternalServerError,
		ContentLength: 6,
		Body:          ioutil.NopCloser(strings.NewReader("whoops")),
	}
	failedRequest, err := parseRequestFromProxyResponse("uh", "oh", mockFailedProxyResponse)
	if failedRequest != nil {
		t.Fatal("Unexpected response for failed proxy response")
	}
	if err == nil {
		t.Fatal("Failed to report proxy error")
	}

	mockForwardedRequest, err := http.NewRequest(http.MethodGet, "/", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	requestReader, requestWriter := io.Pipe()
	go func() {
		mockForwardedRequest.Write(requestWriter)
		requestWriter.Close()
	}()
	mockResponseHeader := make(http.Header)
	mockProxyResponse := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: 0,
		Header:        mockResponseHeader,
		Body:          requestReader,
	}
	mockResponseHeader.Add(HeaderUserID, "someone")
	mockStartTime := time.Now()
	mockResponseHeader.Add(HeaderRequestStartTime, mockStartTime.Format(time.RFC3339Nano))

	forwardedRequest, err := parseRequestFromProxyResponse("some-backend", "some-request", mockProxyResponse)
	if err != nil {
		t.Fatal(err)
	}
	if forwardedRequest.BackendID != "some-backend" || forwardedRequest.RequestID != "some-request" || forwardedRequest.User != "someone" || !(forwardedRequest.StartTime.Equal(mockStartTime)) {
		t.Fatal("Unexpected request parsed from a proxy response")
	}
}

func TestResponseForwarder(t *testing.T) {
	const (
		backendID      = "backend"
		requestID      = "request"
		endUserMessage = "hello"
		backendMessage = "ok"
	)
	expectedResponse := strings.Join([]string{endUserMessage, backendMessage}, "\n")
	endUserRequest := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(endUserMessage))
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := http.ReadResponse(bufio.NewReader(r.Body), endUserRequest)
		if err != nil {
			t.Errorf("Failure reading the proxied response: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		responseBytes, err := ioutil.ReadAll(response.Body)
		if err != nil {
			t.Errorf("Failure reading the response body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if got, want := string(responseBytes), expectedResponse; got != want {
			t.Errorf("Unexpected backend response; got %q, want %q", got, want)
		}
		w.Write([]byte("ok"))
	}))
	defer proxyServer.Close()
	proxyClient := proxyServer.Client()
	responseForwarder, err := NewResponseForwarder(proxyClient, nil, proxyServer.URL+"/", backendID, requestID, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBytes, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Errorf("Failure reading the proxied request: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respMessage := strings.Join([]string{string(requestBytes), backendMessage}, "\n")
		w.Write([]byte(respMessage))
	}))
	defer backendServer.Close()
	backendURL, err := url.Parse(backendServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	hostProxy := httputil.NewSingleHostReverseProxy(backendURL)
	hostProxy.FlushInterval = 100 * time.Millisecond
	hostProxy.ServeHTTP(responseForwarder, endUserRequest)
	if err := responseForwarder.Close(); err != nil {
		t.Errorf("failed to close the response forwarder: %v", err)
	}
}

func TestResponseForwarderWithProxyHangup(t *testing.T) {
	const (
		backendID      = "backend"
		requestID      = "request"
		endUserMessage = "hello"
		backendMessage = "ok"
	)
	endUserRequest := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(endUserMessage))
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ioutil.ReadAll(r.Body)
	}))
	defer proxyServer.Close()
	proxyClient := proxyServer.Client()
	responseForwarder, err := NewResponseForwarder(proxyClient, nil, proxyServer.URL+"/", backendID, requestID, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Kill the proxy server, forcing the response forwarding to fail
		proxyServer.Close()
		w.Write([]byte("ok"))
	}))
	defer backendServer.Close()
	backendURL, err := url.Parse(backendServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	hostProxy := httputil.NewSingleHostReverseProxy(backendURL)
	hostProxy.FlushInterval = 100 * time.Millisecond
	hostProxy.ServeHTTP(responseForwarder, endUserRequest)
	if err := responseForwarder.Close(); err == nil {
		t.Errorf("missing expected error forwarding to a closed proxy")
	}
}

func TestExponentialBackoffDurationDoesntOverflow(t *testing.T) {
	maxRetry := uint(40)

	prevDuration := time.Nanosecond * 0

	for i := uint(0); i < maxRetry; i++ {
		i := ExponentialBackoffDuration(i)

		// Get the minimum acceptable % of the previous retry duration.
		minAcceptablePercentage := float64(1 - 2*JitterPercent)

		// Have to multiply and divide by 100 because time.Duration can't handle
		// floats
		if i < (prevDuration*time.Duration(minAcceptablePercentage*100))/100 {
			t.Errorf("duration shrank too much. Old: %d, Curr: %d", prevDuration, i)
		}

		prevDuration = i
	}
}

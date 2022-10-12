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
	"bytes"
	"errors"
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
	emptyPendingRequests, err := parseRequestIDs(mockEmptyPendingRequestsResponse, nil)
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
	noPendingRequests, err := parseRequestIDs(mockNoPendingRequestsResponse, nil)
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
	failedPendingRequests, err := parseRequestIDs(mockFailedPendingRequestsResponse, nil)
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
	normalPendingRequests, err := parseRequestIDs(mockNormalPendingRequestsResponse, nil)
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
	failedRequest, err := parseRequestFromProxyResponse("uh", "oh", mockFailedProxyResponse, nil)
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

	forwardedRequest, err := parseRequestFromProxyResponse("some-backend", "some-request", mockProxyResponse, nil)
	if err != nil {
		t.Fatal(err)
	}
	if forwardedRequest.BackendID != "some-backend" || forwardedRequest.RequestID != "some-request" || forwardedRequest.User != "someone" || !(forwardedRequest.StartTime.Equal(mockStartTime)) {
		t.Fatal("Unexpected request parsed from a proxy response")
	}
}

func TestReadRequestWithRetries(t *testing.T) {
	mockFwdReq, err := http.NewRequest(http.MethodGet, "/", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	try := 0
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/agent/request"; got != want {
			t.Errorf("Wrong request path: got %v, want %v", got, want)
			http.Error(w, "Wrong request path", http.StatusInternalServerError)
			return
		}
		try++
		if try == 1 {
			// Simulate an error on first attempt - violating redirect policy.
			http.Redirect(w, r, r.URL.String(), http.StatusMovedPermanently)
			return
		}
		if try == 2 {
			// Simulate an error on second attempt - proxy internal error.
			http.Error(w, "Proxy internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set(HeaderUserID, "someone")
		w.Header().Set(HeaderRequestStartTime, time.Now().Format(time.RFC3339Nano))
		w.WriteHeader(http.StatusOK)
		mockFwdReq.Write(w)
	}))
	defer proxyServer.Close()
	proxyClient := proxyServer.Client()
	proxyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return errors.New("no redirects")
	}
	proxyHost := proxyServer.URL + "/"
	var gotFwdReq *ForwardedRequest
	callback := func(client *http.Client, fr *ForwardedRequest) error {
		gotFwdReq = fr
		return nil
	}

	err = ReadRequest(proxyClient, proxyHost, "backend", "request", callback, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := gotFwdReq.User, "someone"; got != want {
		t.Errorf("Unexpected user in read request: got %v, want %v", got, want)
	}
}

func TestBufferedReadSeeker(t *testing.T) {
	r := bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9})
	brs := newBufferedReadSeeker(r, 5)
	p1 := make([]byte, 4)
	p2 := make([]byte, 6)
	wantN := 2
	wantO := int64(0)

	// Pass 1: read 2 + 2 = 4 bytes, which is less than buffer size.
	if gotN, gotErr := brs.Read(p1[0:2]); gotN != wantN || gotErr != nil {
		t.Errorf("Read(pass 1 part 1): got %v, %v; want %v, %v", gotN, gotErr, wantN, nil)
	}
	if gotN, gotErr := brs.Read(p1[2:4]); gotN != wantN || gotErr != nil {
		t.Errorf("Read(pass 1 part 2): got %v, %v; want %v, %v", gotN, gotErr, wantN, nil)
	}
	// Reset offset to 0: succeeds because the buffer has not been filled up.
	if gotO, gotErr := brs.Seek(0, io.SeekStart); gotO != wantO || gotErr != nil {
		t.Errorf("Seek(buffer not filled up): got %v, %v; want %v, %v", gotO, gotErr, wantO, nil)
	}
	// Pass 2: read 2 + 2 + 2 = 6 bytes, which exceeds buffer size.
	if gotN, gotErr := brs.Read(p2[0:2]); gotN != wantN || gotErr != nil {
		t.Errorf("Read(pass 2 part 1): got %v, %v; want %v, %v", gotN, gotErr, wantN, nil)
	}
	if gotN, gotErr := brs.Read(p2[2:4]); gotN != wantN || gotErr != nil {
		t.Errorf("Read(pass 2 part 2): got %v, %v; want %v, %v", gotN, gotErr, wantN, nil)
	}
	if gotN, gotErr := brs.Read(p2[4:6]); gotN != wantN || gotErr != nil {
		t.Errorf("Read(pass 2 part 3): got %v, %v; want %v, %v", gotN, gotErr, wantN, nil)
	}
	// Reset offset to 0: fails because the buffer has been filled up.
	if gotO, gotErr := brs.Seek(0, io.SeekStart); gotErr == nil {
		t.Errorf("Seek(buffer filled up): got %v, %v; want error", gotO, gotErr)
	}

	if got, want := p1, []byte{1, 2, 3, 4}; !bytes.Equal(got, want) {
		t.Errorf("Unexpected values read in pass 1: got %v, want %v", got, want)
	}
	if got, want := p2, []byte{1, 2, 3, 4, 5, 6}; !bytes.Equal(got, want) {
		t.Errorf("Unexpected values read in pass 2: got %v, want %v", got, want)
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
	responseForwarder, err := NewResponseForwarder(proxyClient, proxyServer.URL+"/", backendID, requestID)
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
	responseForwarder, err := NewResponseForwarder(proxyClient, proxyServer.URL+"/", backendID, requestID)
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

func TestResponseForwarderWithRetries(t *testing.T) {
	endUserRequest := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	try := 0
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		try++
		if try == 1 {
			// Simulate an error on first attempt - violating redirect policy.
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		if try == 2 {
			// Simulate an error on second attempt - proxy internal error.
			http.Error(w, "Proxy internal error", http.StatusInternalServerError)
			return
		}
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
		if got, want := string(responseBytes), "test backend response"; got != want {
			t.Errorf("Unexpected backend response; got %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxyServer.Close()
	proxyClient := proxyServer.Client()
	proxyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return errors.New("no redirects")
	}
	responseForwarder, err := NewResponseForwarder(proxyClient, proxyServer.URL+"/", "backend", "request")
	if err != nil {
		t.Fatal(err)
	}

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("test backend response"))
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

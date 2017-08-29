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
	"io"
	"io/ioutil"
	"net/http"
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

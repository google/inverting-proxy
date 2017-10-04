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

// Package utils defines utilities for the agent.
package utils

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
)

const (
	// PendingPath is the URL subpath for pending requests held by the proxy.
	PendingPath = "agent/pending"

	// RequestPath is the URL subpath for reading a specific request held by the proxy.
	RequestPath = "agent/request"

	// ResponsePath is the URL subpath for posting a request response to the proxy.
	ResponsePath = "agent/response"

	// HeaderUserID is the name of a response header used by the proxy to identify the end user.
	HeaderUserID = "X-Inverting-Proxy-User-ID"

	// HeaderBackendID is the name of a request header used to uniquely identify this agent.
	HeaderBackendID = "X-Inverting-Proxy-Backend-ID"

	// HeaderVMID is the name of a request header used to report the VM
	// (if any) on which the agent is running.
	HeaderVMID = "X-Inverting-Proxy-VM-ID"

	// HeaderRequestID is the name of a request/response header used to uniquely
	// identify a proxied request.
	HeaderRequestID = "X-Inverting-Proxy-Request-ID"

	// HeaderRequestStartTime is the name of a response header used by the proxy
	// to report the start time of a proxied request.
	HeaderRequestStartTime = "X-Inverting-Proxy-Request-Start-Time"
)

// hopHeaders are Hop-by-hop headers. These are removed when received in a response from
// the backend. For details, see: http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true, // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                true, // canonicalized version of "TE"
	"Trailer":           true, // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding": true,
	"Upgrade":           true,
}

// PendingRequests represents a list of request IDs that do not yet have a response.
type PendingRequests []string

// ForwardedRequest represents an end-client HTTP request that was forwarded
// to us by the inverting proxy.
type ForwardedRequest struct {
	BackendID string
	RequestID string
	User      string
	StartTime time.Time

	Contents *http.Request
}

// RequestCallback defines how the caller of `ReadRequest` uses the request that was read.
//
// This is done as a callback so that the caller of `ReadRequest` does not have to remember
// to call `Close()` on the nested *http.Request object's body.
type RequestCallback func(client *http.Client, fr *ForwardedRequest) error

// parseRequestIDs takes a response from the proxy and parses any forwarded request IDs out of it.
func parseRequestIDs(response *http.Response) ([]string, error) {
	responseBody := &io.LimitedReader{
		R: response.Body,
		// If a response is larger than 1MB, then truncate it. This will result in an
		// failure to parse the result, but that is better than a potential OOM.
		//
		// Note that this shouldn't happen anyway, since a reasonable proxy server
		// should limit the size of a response to less than this. For instance, the
		// initial version of our proxy will never return a list of more than 100
		// request IDs.
		N: 1024 * 1024,
	}
	responseBytes, err := ioutil.ReadAll(responseBody)
	if err != nil {
		return nil, fmt.Errorf("Failed to read the forwarded request: %q\n", err.Error())
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed to list pending requests: %d, %q", response.StatusCode, responseBytes)
	}
	if len(responseBytes) <= 0 {
		return []string{}, nil
	}

	var requests []string
	if err := json.Unmarshal(responseBytes, &requests); err != nil {
		return nil, fmt.Errorf("Failed to parse the requests: %q\n", err.Error())
	}
	return requests, nil
}

var (
	hasVMIDOnce sync.Once
	hasVMID     bool
)

func hasVMServiceAccount() bool {
	if !metadata.OnGCE() {
		return false
	}

	if _, err := metadata.Get("instance/service-accounts/default/email"); err != nil {
		return false
	}
	return true
}

func checkVMID() {
	// VM Identity requires a service account.
	hasVMID = hasVMServiceAccount()
}

// addVMIDHeader adds a header to the given request identifying the VM (if any) on which the agent is running.
//
// This method relies on the Google Compute Engine functionality for verifying a VM's identity
// (https://cloud.google.com/compute/docs/instances/verifying-instance-identity), so it does
// nothing if the agent is not running inside of a Google Compute Engine VM.
func addVMIDHeader(proxyURL string, req *http.Request) error {
	hasVMIDOnce.Do(checkVMID)
	if !hasVMID {
		return nil
	}

	idPath := fmt.Sprintf("instance/service-accounts/default/identity?format=full&audience=%s", proxyURL)
	vmID, err := metadata.Get(idPath)
	if err != nil {
		return err
	}
	req.Header.Add(HeaderVMID, vmID)
	return nil
}

// ListPendingRequests issues a single request to the proxy to ask for the IDs of pending requests.
func ListPendingRequests(client *http.Client, proxyHost, backendID string) ([]string, error) {
	proxyURL := proxyHost + PendingPath
	proxyReq, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Add(HeaderBackendID, backendID)
	if err := addVMIDHeader(proxyURL, proxyReq); err != nil {
		return nil, fmt.Errorf("Failure adding the VM ID header to a proxy request: %q", err.Error())
	}
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("A proxy request failed: %q", err.Error())
	}
	defer proxyResp.Body.Close()
	return parseRequestIDs(proxyResp)
}

func parseRequestFromProxyResponse(backendID, requestID string, proxyResp *http.Response) (*ForwardedRequest, error) {
	user := proxyResp.Header.Get(HeaderUserID)
	startTimeStr := proxyResp.Header.Get(HeaderRequestStartTime)

	if proxyResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Error status while reading %q from the proxy", requestID)
	}

	startTime, err := time.Parse(time.RFC3339Nano, startTimeStr)
	if err != nil {
		return nil, err
	}

	contents, err := http.ReadRequest(bufio.NewReader(proxyResp.Body))
	if err != nil {
		return nil, err
	}
	return &ForwardedRequest{
		BackendID: backendID,
		RequestID: requestID,
		User:      user,
		StartTime: startTime,
		Contents:  contents,
	}, nil
}

// ReadRequest reads a forwarded client request from the inverting proxy.
//
// If the returned request is non-nil, then it is passed to the provided callback.
func ReadRequest(client *http.Client, proxyHost, backendID, requestID string, callback RequestCallback) error {
	proxyURL := proxyHost + RequestPath
	proxyReq, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		return err
	}
	proxyReq.Header.Add(HeaderBackendID, backendID)
	proxyReq.Header.Add(HeaderRequestID, requestID)
	if err := addVMIDHeader(proxyURL, proxyReq); err != nil {
		return fmt.Errorf("Failure adding the VM ID header to a proxy request: %q", err.Error())
	}
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("A proxy request failed: %q", err.Error())
	}
	defer proxyResp.Body.Close()

	fr, err := parseRequestFromProxyResponse(backendID, requestID, proxyResp)
	if err != nil {
		return err
	}
	return callback(client, fr)
}

// ResponseForwarder implements http.ResponseWriter by dumping a wire-compatible
// representation of the response to 'proxyWriter' field.
//
// ResponseForwarder is used by the agent to forward a response from the backend
// target to the inverting proxy.
type ResponseForwarder struct {
	proxyWriter        *io.PipeWriter
	startedChan        chan struct{}
	responseBodyWriter *io.PipeWriter

	// wroteHeader is set when WriteHeader is called. It's used to ensure a
	// call to WriteHeader before the first call to Write.
	wroteHeader bool

	// response is synthesized using the backend target response. We use its Write
	// method as a convenience when forwarding the wire-representation received
	// by the backend target.
	response *http.Response

	// errors is a channel where all internal errors from proxying a request/response
	// get written. This is eventually returned to the caller of the Close method.
	errors chan error
}

// NewResponseForwarder constructs a new ResponseForwarder that forwards to the
// given proxy for the specified request.
func NewResponseForwarder(client *http.Client, proxyHost, backendID, requestID string) (*ResponseForwarder, error) {
	// The contortions below support streaming.
	//
	// There are two pipes:
	// 1. proxyReader, proxyWriter: The io.PipeWriter for the HTTP POST to the inverting proxy.
	//       To this pipe, we write the full HTTP response from the backend target in HTTP
	//       wire-format form. (Status + Headers + Body + Trailers)
	//
	// 2. responseBodyReader, responseBodyWriter: This pipe corresponds to the response body
	//       from the backend target. To this pipe, we stream each read from backend target.
	proxyReader, proxyWriter := io.Pipe()
	startedChan := make(chan struct{}, 1)
	responseBodyReader, responseBodyWriter := io.Pipe()

	proxyURL := proxyHost + ResponsePath
	proxyReq, err := http.NewRequest(http.MethodPost, proxyURL, proxyReader)
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Set(HeaderBackendID, backendID)
	proxyReq.Header.Set(HeaderRequestID, requestID)
	if err := addVMIDHeader(proxyURL, proxyReq); err != nil {
		return nil, fmt.Errorf("Failure adding the VM ID header to a proxy request: %q", err.Error())
	}
	proxyReq.Header.Set("Content-Type", "text/plain")

	errChan := make(chan error, 100)
	go func() {
		// Wait until the response body has started being written
		// (for a non-empty response) or for the response to
		// be closed (for an empty response) before triggering
		// the proxy request round trip.
		//
		// This ensures that we do not fetch the bearer token
		// for the auth header until the last possible moment.
		// That, in turn. prevents a race condition where the
		// token expires between the header being generated
		// and the request being sent to the proxy.
		<-startedChan

		if _, err := client.Do(proxyReq); err != nil {
			errChan <- err
		}

		// The fact that the client.Do call has returned tells
		// us that the proxyReader end of the pipe has returned
		// an EOF. That, in turn, tells us that `Close` has been
		// called on the proxyWriter end of the pipe. We don't
		// call `Close` on the proxyWriter until after writing
		// the response to it, so we similarly know that `Close`
		// has been called on the responseBodyWriter.
		//
		// All of this means that we cannot get to this place in
		// the code until `Close` as been called on the
		// ResponseForwarder and the backend response has been
		// forwarded to the proxy server. As such, we can now
		// safely close the error channel.
		close(errChan)
	}()

	return &ResponseForwarder{
		response: &http.Response{
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       responseBodyReader,
		},
		wroteHeader:        false,
		proxyWriter:        proxyWriter,
		startedChan:        startedChan,
		responseBodyWriter: responseBodyWriter,
		errors:             errChan,
	}, nil
}

func (rf *ResponseForwarder) notify() {
	if rf.startedChan != nil {
		rf.startedChan <- struct{}{}
		rf.startedChan = nil
	}
}

// Header implements the http.ResponseWriter interface.
func (rf *ResponseForwarder) Header() http.Header {
	return rf.response.Header
}

// Write implements the http.ResponseWriter interface.
func (rf *ResponseForwarder) Write(buf []byte) (int, error) {
	// As in net/http, call WriteHeader if it has not yet been called
	// before the first call to Write.
	if !rf.wroteHeader {
		rf.WriteHeader(http.StatusOK)
	}
	rf.notify()
	count, err := rf.responseBodyWriter.Write(buf)
	if err != nil {
		rf.errors <- err
	}
	return count, err
}

// WriteHeader implements the http.ResponseWriter interface.
func (rf *ResponseForwarder) WriteHeader(code int) {
	// As in net/http, ignore multiple calls to WriteHeader.
	if rf.wroteHeader {
		return
	}
	rf.wroteHeader = true
	for k, v := range rf.response.Header {
		if _, ok := hopHeaders[k]; ok {
			continue
		}
		rf.response.Header[k] = v
	}
	rf.response.StatusCode = code
	rf.response.Status = http.StatusText(rf.response.StatusCode)
	// This will write the status and headers immediately and stream the
	// body using the pipes we've wired.
	go func() {
		defer rf.proxyWriter.Close()
		if err := rf.response.Write(rf.proxyWriter); err != nil {
			rf.errors <- err

			// Normally, the end of this goroutine indicates
			// that the response.Body reader has returned an EOF,
			// which means that the corresponding writer has been
			// closed. However, that is not necessarily the case
			// if we hit an error in the call to `Write`.
			//
			// In this case, there may still be someone writing
			// to the pipe writer, but we will no longer be reading
			// anything from the corresponding reader. As such,
			// we signal that issue to any remaining writers.
			rf.response.Body.(*io.PipeReader).CloseWithError(err)
		}
	}()
}

// Close signals that the response has been fully read from the backend server,
// waits for that response to be forwarded to the proxy, and then reports any
// errors that occured while forwarding the response.
func (rf *ResponseForwarder) Close() error {
	rf.notify()
	var errs []error
	if err := rf.responseBodyWriter.Close(); err != nil {
		errs = append(errs, err)
	}
	for err := range rf.errors {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("Multiple errors closing pipe writers: %s", errs)
	}
	return nil
}

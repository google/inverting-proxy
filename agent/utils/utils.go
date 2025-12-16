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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/google/inverting-proxy/agent/metrics"
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

	// JitterPercent sets the jitter for exponential backoff retry time
	JitterPercent = 0.1

	// Max time to wait before retry during exponential backoff
	maxBackoffDuration = 3 * time.Second

	// Time to wait on first retry
	firstRetryWaitDuration = time.Millisecond

	// Max number of retries to perform in case of failed read request calls
	maxReadRequestRetryCount = 2

	// Max number of retries to perform in case of failed write response calls
	maxWriteResponseRetryCount = 2

	// Size of the buffer that stores the beginning of read response body
	readResponseBufSize = 4096
)

var (
	// compute the max retry count
	maxRetryCount = math.Log2(float64(maxBackoffDuration / firstRetryWaitDuration))
)

// hopHeaders are Hop-by-hop headers. These are removed when received in a response from
// the backend. For details, see: http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true, // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true, // canonicalized version of "TE"
	"Trailer":             true, // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding":   true,
	"Upgrade":             true,
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
func parseRequestIDs(response *http.Response, metricHandler *metrics.MetricHandler) ([]string, error) {
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
		return nil, fmt.Errorf("failed to read the forwarded request: %q", err.Error())
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list pending requests: %d, %q", response.StatusCode, responseBytes)
	}
	if len(responseBytes) <= 0 {
		return []string{}, nil
	}

	var requests []string
	if err := json.Unmarshal(responseBytes, &requests); err != nil {
		return nil, fmt.Errorf("failed to parse the requests: %q", err.Error())
	}
	return requests, nil
}

func hasVMServiceAccount() bool {
	if !metadata.OnGCE() {
		return false
	}

	if _, err := metadata.Get("instance/service-accounts/default/email"); err != nil {
		return false
	}
	return true
}

func getVMID(audience string) string {
	for {
		idPath := fmt.Sprintf("instance/service-accounts/default/identity?format=full&audience=%s", audience)
		vmID, err := metadata.Get(idPath)
		if err == nil {
			return vmID
		}
		log.Printf("failure fetching a VM ID: %v", err)
	}
}

type vmTransport struct {
	wrapped http.RoundTripper

	// Protects the `currID` field below
	sync.Mutex
	currID string
}

func (t *vmTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.Lock()
	id := t.currID
	t.Unlock()
	r.Header.Add(HeaderVMID, id)
	return t.wrapped.RoundTrip(r)
}

// RoundTripperWithVMIdentity returns an http.RoundTripper that includes a GCE VM ID token in
// every outbound request. The token is fetched from the metadata server and
// stored in the 'X-Inverting-Proxy-VM-ID' header.
//
// This method relies on the Google Compute Engine functionality for verifying a VM's identity
// (https://cloud.google.com/compute/docs/instances/verifying-instance-identity), so it if this
// is not running inside of a Google Compute Engine VM, then it just returns the passed in RoundTripper.
func RoundTripperWithVMIdentity(ctx context.Context, wrapped http.RoundTripper, proxyURL string, disableGCEVM bool) http.RoundTripper {
	if !hasVMServiceAccount() || disableGCEVM {
		return wrapped
	}

	transport := &vmTransport{
		wrapped: wrapped,
		currID:  getVMID(proxyURL),
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nextID := getVMID(proxyURL)
				transport.Lock()
				transport.currID = nextID
				transport.Unlock()
			}
		}
	}()
	return transport
}

// ListPendingRequests issues a single request to the proxy to ask for the IDs of pending requests.
func ListPendingRequests(ctx context.Context, client *http.Client, proxyHost, backendID string, metricHandler *metrics.MetricHandler) ([]string, error) {
	proxyURL := proxyHost + PendingPath
	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL, nil)
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Add(HeaderBackendID, backendID)
	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("A proxy request failed: %q", err.Error())
	}
	defer proxyResp.Body.Close()
	return parseRequestIDs(proxyResp, metricHandler)
}

func getRequestWithRetries(client *http.Client, proxyURL, backendID, requestID string) (*http.Response, error) {
	proxyReq, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Add(HeaderBackendID, backendID)
	proxyReq.Header.Add(HeaderRequestID, requestID)
	var proxyResp *http.Response
	for retryCount := 0; retryCount <= maxReadRequestRetryCount; retryCount++ {
		proxyResp, err = client.Do(proxyReq)
		if err != nil {
			continue
		}
		if 500 <= proxyResp.StatusCode && proxyResp.StatusCode < 600 {
			proxyResp.Body.Close()
			continue
		}
		return proxyResp, nil
	}
	return proxyResp, err
}

func parseRequestFromProxyResponse(backendID, requestID string, proxyResp *http.Response, metricHandler *metrics.MetricHandler) (*ForwardedRequest, error) {
	user := proxyResp.Header.Get(HeaderUserID)
	startTimeStr := proxyResp.Header.Get(HeaderRequestStartTime)
	go metricHandler.WriteResponseCodeMetric(proxyResp.StatusCode)

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
func ReadRequest(client *http.Client, proxyHost, backendID, requestID string, callback RequestCallback, metricHandler *metrics.MetricHandler) error {
	proxyURL := proxyHost + RequestPath
	proxyResp, err := getRequestWithRetries(client, proxyURL, backendID, requestID)
	if err != nil {
		return fmt.Errorf("A proxy request failed: %q", err.Error())
	}
	defer proxyResp.Body.Close()

	fr, err := parseRequestFromProxyResponse(backendID, requestID, proxyResp, metricHandler)
	if err != nil {
		return err
	}
	return callback(client, fr)
}

func newBufferedReadSeeker(r io.Reader, bufSize int) *bufferedReadSeeker {
	return &bufferedReadSeeker{
		r:         r,
		buf:       make([]byte, bufSize),
		writeHead: 0,
		readHead:  0,
	}
}

type bufferedReadSeeker struct {
	r         io.Reader
	buf       []byte
	writeHead int
	readHead  int
}

func (b *bufferedReadSeeker) Read(p []byte) (int, error) {
	// Read from buffer.
	readFromBuf := copy(p, b.buf[b.readHead:b.writeHead])
	b.readHead += readFromBuf
	// Read from wrapped source and write to buffer.
	readFromSource, err := b.r.Read(p[readFromBuf:])
	written := copy(b.buf[b.writeHead:], p[readFromBuf:(readFromBuf+readFromSource)])
	b.writeHead += written
	b.readHead += written
	return readFromBuf + readFromSource, err
}

func (b *bufferedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, errors.New("only SeekStart offset is supported")
	}
	if offset < 0 || offset >= int64(len(b.buf)) {
		return 0, errors.New("invalid offset value")
	}
	if b.writeHead >= len(b.buf) {
		return 0, errors.New("cannot seek, possible buffer overflow")
	}
	b.readHead = int(offset)
	return int64(b.readHead), nil
}

func postResponseWithRetries(client *http.Client, proxyURL, backendID, requestID string, proxyReader io.Reader) error {
	proxyReadSeeker := newBufferedReadSeeker(proxyReader, readResponseBufSize)
	proxyReq, err := http.NewRequest(http.MethodPost, proxyURL, proxyReadSeeker)
	if err != nil {
		return err
	}
	proxyReq.Header.Set(HeaderBackendID, backendID)
	proxyReq.Header.Set(HeaderRequestID, requestID)
	proxyReq.Header.Set("Content-Type", "text/plain")
	var proxyResp *http.Response
	for retryCount := 0; retryCount <= maxWriteResponseRetryCount; retryCount++ {
		if proxyResp, err = client.Do(proxyReq); err != nil {
			if _, seekErr := proxyReadSeeker.Seek(0, io.SeekStart); seekErr != nil {
				return err
			}
			continue
		}
		// Force the response body to be fully read by copying it to the discard target.
		//
		// The response is expected to be empty for this specific case, so reading the entire
		// response body should be safe and not cause any delays.
		//
		// The reason we do this is because under certain conditions (in particular, when
		// using HTTP/2) the server and client might both try to close the underlying stream
		// at the same time. When that happens, the client sends a PING message at the
		// same time it sends the `RST_STREAM` message.
		//
		// If this happens a lot, it can trigger protections that a lot of server deployments
		// have against PING flood attacks.
		//
		// Draining the response body forces the server-side `END_STREAM` message to
		// arrive before the client tries to send the `RST_STREAM` message. In that case,
		// the client will not send a PING and so will not trigger these PING flood protections.
		//
		// A future version of the Go standard library will likely change the client behavior
		// so that it is more conservative about sending these PING messages, and if
		// that happens then this line can be removed. The proposed change for that
		// is https://go-review.git.corp.google.com/c/net/+/720300
		io.Copy(io.Discard, proxyResp.Body)
		proxyResp.Body.Close()
		if 500 <= proxyResp.StatusCode && proxyResp.StatusCode < 600 {
			if _, seekErr := proxyReadSeeker.Seek(0, io.SeekStart); seekErr != nil {
				return err
			}
			continue
		}
		return nil
	}
	return err
}

// ResponseWriteCloser combines the http.ResponseWriter and io.Closer interfaces.
type ResponseWriteCloser interface {
	http.ResponseWriter
	io.Closer
}

// StreamingResponseWriter is a ResponseWriteCloser with an additional method to notify
// writers that the streamed response is no longer being consumed due to an error.
type StreamingResponseWriter interface {
	ResponseWriteCloser

	// CloseWithError signals to any clients of the ResponseWriter that further
	// writes to the response are not being processed, and specifies the error that
	// should be returned to those writers to indicate this.
	//
	// This should only be called by the code that is processing the written response.
	CloseWithError(error) error
}

// NewStreamingResponseWriter returns a ResponseWriteCloser that forwards the written
// response to the given channel.
//
// The response is sent to the given channel as soon as the status code is set, the response
// body is streamed as soon as each successive call to the ResponseWriter's `Write` method is
// invoked, and the trailers are made available once the `Close` method is called.
//
// The caller that passes this ResponseWriter to an http.Handler is responsible for closing
// the writer as soon as the call to the handler's `ServeHTTP` method completes.
//
// If the caller hits an error before reading all of the streamed returned response's body,
// then it should call `CloseWithError` to propogate this error to any remaining writers
// of the response body.
//
// Any hop-by-hop headers are filtered out from both the response header and trailer.
func NewStreamingResponseWriter(respChan chan *http.Response, r *http.Request) StreamingResponseWriter {
	bodyReader, bodyWriter := io.Pipe()
	return &streamingResponseWriter{
		r:          r,
		respChan:   respChan,
		header:     make(http.Header),
		trailer:    make(http.Header),
		bodyWriter: bodyWriter,
		bodyReader: bodyReader,
	}
}

type streamingResponseWriter struct {
	r           *http.Request
	respChan    chan *http.Response
	header      http.Header
	trailer     http.Header
	wroteHeader bool
	bodyWriter  *io.PipeWriter
	bodyReader  *io.PipeReader
}

func (w *streamingResponseWriter) Header() http.Header {
	return w.header
}

func (w *streamingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	// Initialize the response trailers.
	w.trailer = make(http.Header)
	for _, k := range w.Header().Values("Trailer") {
		// Initialize trailers with empty slices for any pre-declared values.
		//
		// This is necessary for the httputil.ReverseProxy type to forward them correctly.
		// See [here](https://github.com/golang/go/blob/5e3c4016a436c357a57a6f7870913c6911c6904e/src/net/http/httputil/reverseproxy.go#L509)
		if _, ok := hopHeaders[k]; ok {
			continue
		}
		// We manually call `CanonicalHeaderKey` to preserve the invariant that
		// all keys in a `Header` instance must be in their canonical format.
		w.trailer[http.CanonicalHeaderKey(k)] = []string{}
	}

	// Filter out hop-by-hop headers.
	header := make(http.Header)
	for k, vs := range w.Header() {
		if _, ok := hopHeaders[k]; ok {
			continue
		}
		// Iterate through the values to ensure that subsequent changes to the
		// headers map do not affect the streamed response.
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	w.header = header

	// Take the protocol version information for the response from the corresponding request.
	proto := "HTTP/1.1"
	protoMajor := 1
	protoMinor := 1
	if w.r != nil {
		proto = w.r.Proto
		protoMajor = w.r.ProtoMajor
		protoMinor = w.r.ProtoMinor
	}
	resp := &http.Response{
		Proto:      proto,
		ProtoMajor: protoMajor,
		ProtoMinor: protoMinor,
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     w.header,
		Body:       w.bodyReader,
		Trailer:    w.trailer,
	}
	select {
	case w.respChan <- resp:
	case <-w.r.Context().Done():
		w.bodyReader.Close()
	}
}

func (w *streamingResponseWriter) Write(bs []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.bodyWriter.Write(bs)
}

func (w *streamingResponseWriter) Close() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	for k, _ := range w.trailer {
		for _, v := range w.Header().Values(k) {
			// The `Values` method does not return a copy, so we manually
			// add each value one at a time to ensure that subsequent changes
			// to the header do not affect the trailers map.
			w.trailer.Add(k, v)
		}
	}
	for k, vs := range w.Header() {
		var foundPrefix bool
		k, foundPrefix = strings.CutPrefix(k, http.TrailerPrefix)
		if !foundPrefix {
			continue
		}
		if _, ok := hopHeaders[k]; ok {
			continue
		}
		for _, v := range vs {
			w.trailer.Add(k, v)
		}
	}
	return w.bodyWriter.Close()
}

func (w *streamingResponseWriter) CloseWithError(err error) error {
	return w.bodyReader.CloseWithError(err)
}

// NewResponseForwarder constructs a new ResponseWriteCloser that forwards to the
// given proxy for the specified request.
func NewResponseForwarder(client *http.Client, proxyHost, backendID, requestID string, r *http.Request, metricHandler *metrics.MetricHandler) (ResponseWriteCloser, error) {
	// Construct a streaming response writer so we can process the written response
	// in separate goroutines as it is written.
	respChan := make(chan *http.Response)
	rw := NewStreamingResponseWriter(respChan, r)

	// Construct two ends of a pipe that we will use for concurrently writing the response
	// to the proxy:
	//
	// 1. proxyWriter is used to write out the wire-format of the streamed response to
	//    the body of the request we forward to the proxy.
	//
	//    This is done in a separate goroutine because the `.Write(...)` call on the
	//    streamed response blocks while the body of the response from the backend is
	//    being written.
	// 2. proxyReader is used to read in the body of the request that we forward to the
	//    proxy.
	//
	//    This is done in a separate goroutine because the call to post the request
	//    to the proxy blocks on that HTTP roundtrip.
	proxyReader, proxyWriter := io.Pipe()

	// Write the request to the proxy in a separate goroutine.
	postErrChan := make(chan error, 1)
	go func() {
		defer proxyReader.Close()
		defer close(postErrChan)
		proxyURL := proxyHost + ResponsePath
		if err := postResponseWithRetries(client, proxyURL, backendID, requestID, proxyReader); err != nil {
			postErrChan <- err
		}
	}()

	// Write out the wire-format of the streamed response from the backend in order
	// to be able to forward it to the proxy.
	writeErrChan := make(chan error, 1)
	go func() {
		defer proxyWriter.Close()
		defer close(writeErrChan)
		var statusCode int
		select {
		case <-r.Context().Done():
			// The request was cancelled before we received a response.
		case resp := <-respChan:
			// Force the transfer encoding to chunked so that the response writing
			// is performed incrementally as response data is available.
			resp.TransferEncoding = []string{"chunked"}
			if err := resp.Write(proxyWriter); err != nil {
				rw.CloseWithError(err)
				writeErrChan <- err
			}
			statusCode = resp.StatusCode
		}
		go metricHandler.WriteResponseCodeMetric(statusCode)
	}()
	return &responseForwarder{rw, postErrChan, writeErrChan}, nil
}

type responseForwarder struct {
	ResponseWriteCloser

	postErrChan  chan error
	writeErrChan chan error
}

func (r *responseForwarder) Close() error {
	if err := r.ResponseWriteCloser.Close(); err != nil {
		return err
	}
	if err := <-r.postErrChan; err != nil {
		return err
	}
	if err := <-r.writeErrChan; err != nil {
		return err
	}
	return nil
}

// ExponentialBackoffDuration gets time to wait before retry for exponential
// backoff
func ExponentialBackoffDuration(retryCount uint) time.Duration {
	var targetDuration time.Duration
	if retryCount > uint(maxRetryCount) {
		targetDuration = maxBackoffDuration
	} else {
		targetDuration = (1 << retryCount) * firstRetryWaitDuration
	}

	targetDuration = addJitter(targetDuration, JitterPercent)
	return targetDuration
}

func addJitter(duration time.Duration, jitterPercent float64) time.Duration {
	jitter := 1 - jitterPercent + rand.Float64()*(jitterPercent*2)
	return time.Duration(float64(duration.Nanoseconds())*jitter) * time.Nanosecond
}

func ShutdownSignalChan() <-chan struct{} {
	// Listen for shutdown signal.
	sigs := make(chan os.Signal, 1)
	ch := make(chan struct{})

	// SIGINT: The SIGINT (“program interrupt”) signal is sent when the user types the INTR character (Ctrl+C)
	// SIGTERM: The SIGTERM signal is a generic signal used to cause program termination
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		close(ch)
	}()
	return ch
}

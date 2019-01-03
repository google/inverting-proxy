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

package agent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/inverting-proxy/agent/utils"
	"golang.org/x/net/context"
)

// pendingRequest represents a frontend request
type pendingRequest struct {
	startTime time.Time
	req       *http.Request
	respChan  chan *http.Response
}

func newPendingRequest(r *http.Request) *pendingRequest {
	return &pendingRequest{
		startTime: time.Now(),
		req:       r,
		respChan:  make(chan *http.Response),
	}
}

type proxy struct {
	t             *testing.T
	requestIDs    chan string
	randGenerator *rand.Rand

	// protects the map below
	sync.Mutex
	requests map[string]*pendingRequest
}

func newProxy(t *testing.T) *proxy {
	return &proxy{
		t:             t,
		requestIDs:    make(chan string),
		randGenerator: rand.New(rand.NewSource(time.Now().UnixNano())),
		requests:      make(map[string]*pendingRequest),
	}
}

func (p *proxy) handleAgentPostResponse(w http.ResponseWriter, r *http.Request, requestID string) {
	p.Lock()
	pending, ok := p.requests[requestID]
	p.Unlock()
	if !ok {
		p.t.Logf("Could not find pending request: %q", requestID)
		http.NotFound(w, r)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(r.Body), pending.req)
	if err != nil {
		p.t.Logf("Could not parse response to request %q: %v", requestID, err)
		http.Error(w, "Failure parsing request body", http.StatusBadRequest)
		return
	}
	// We want to track whether or not the body has finished being read so that we can
	// make sure that this method does not return until after that. However, we do not
	// want to block the sending of the response to the client while it is being read.
	//
	// To accommodate both goals, we replace the response body with a pipereader, start
	// forwarding the response immediately, and then copy the original body to the
	// corresponding pipewriter.
	respBody := resp.Body
	defer respBody.Close()

	pr, pw := io.Pipe()
	defer pw.Close()

	resp.Body = pr
	select {
	case <-r.Context().Done():
		return
	case pending.respChan <- resp:
	}
	if _, err := io.Copy(pw, respBody); err != nil {
		p.t.Logf("Could not read response to request %q: %v", requestID, err)
		http.Error(w, "Failure reading request body", http.StatusInternalServerError)
	}
}

func (p *proxy) handleAgentGetRequest(w http.ResponseWriter, r *http.Request, requestID string) {
	p.Lock()
	pending, ok := p.requests[requestID]
	p.Unlock()
	if !ok {
		p.t.Logf("Could not find pending request: %q", requestID)
		http.NotFound(w, r)
		return
	}
	p.t.Logf("Returning pending request: %q", requestID)
	w.Header().Set(utils.HeaderRequestStartTime, pending.startTime.Format(time.RFC3339Nano))
	w.WriteHeader(http.StatusOK)
	pending.req.Write(w)
}

// waitForRequestIDs blocks until at least one request ID is available, and then returns
// a slice of all of the IDs available at that time.
//
// Note that any IDs returned by this method will never be returned again.
func (p *proxy) waitForRequestIDs(ctx context.Context) []string {
	var requestIDs []string
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(30 * time.Second):
		return nil
	case id := <-p.requestIDs:
		requestIDs = append(requestIDs, id)
	}
	for {
		select {
		case id := <-p.requestIDs:
			requestIDs = append(requestIDs, id)
		default:
			return requestIDs
		}
	}
}

func (p *proxy) handleAgentListRequests(w http.ResponseWriter, r *http.Request) {
	requestIDs := p.waitForRequestIDs(r.Context())
	respJSON, err := json.Marshal(requestIDs)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failure serializing the request IDs: %v", err), http.StatusInternalServerError)
		return
	}
	p.t.Logf("Reporting pending requests: %s", respJSON)
	w.WriteHeader(http.StatusOK)
	w.Write(respJSON)
}

func (p *proxy) handleAgentRequest(w http.ResponseWriter, r *http.Request, backendID string) {
	requestID := r.Header.Get(utils.HeaderRequestID)
	if requestID == "" {
		p.t.Logf("Received new backend list request from %q", backendID)
		p.handleAgentListRequests(w, r)
		return
	}
	if r.Method == http.MethodPost {
		p.t.Logf("Received new backend post request from %q", backendID)
		p.handleAgentPostResponse(w, r, requestID)
		return
	}
	p.t.Logf("Received new backend get request from %q", backendID)
	p.handleAgentGetRequest(w, r, requestID)
}

func (p *proxy) newID() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d", p.randGenerator.Int63())))
	return fmt.Sprintf("%x", sum)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if backendID := r.Header.Get(utils.HeaderBackendID); backendID != "" {
		p.handleAgentRequest(w, r, backendID)
		return
	}
	id := p.newID()
	p.t.Logf("Received new frontend request %q", id)
	pending := newPendingRequest(r)
	p.Lock()
	p.requests[id] = pending
	p.Unlock()

	// Enqueue the request
	select {
	case <-r.Context().Done():
		// The client request was cancelled
		p.t.Logf("Timeout waiting to enqueue the request ID for %q", id)
		return
	case p.requestIDs <- id:
	}
	p.t.Logf("Request %q enqueued after %s", id, time.Since(pending.startTime))

	// Pull out and copy the response
	select {
	case <-r.Context().Done():
		// The client request was cancelled
		p.t.Logf("Timeout waiting for the response to %q", id)
		return
	case resp := <-pending.respChan:
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	p.t.Logf("Response for %q received after %s", id, time.Since(pending.startTime))
}

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

	p := httptest.NewServer(newProxy(t))
	go func() {
		<-ctx.Done()
		backend.Close()
		p.Close()
		fakeMetadata.Close()
	}()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("Failed to parse the backend URL: %v", err)
	}
	t.Logf("Started backend at localhost:%s and proxy at %q", backendURL.Port(), p.URL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--backend=testBackend",
		"--proxy", p.URL+"/",
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
	if err := checkRequest(p.URL, testPath, testPath, time.Second); err != nil {
		t.Fatalf("Failed to send the initial request: %v", err)
	}

	for i := 0; i < 10; i++ {
		// The timeout below was chosen to be overly generous to prevent test flakiness.
		//
		// This has the consequence that it will only catch severe latency regressions.
		//
		// The specific value was chosen by running the test in a loop 100 times and
		// incrementing the value until all 100 runs passed.
		if err := checkRequest(p.URL, testPath, testPath, 5*time.Millisecond); err != nil {
			t.Fatalf("Failed to send request %d: %v", i, err)
		}
	}
}

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

// Command server launches a stand-alone inverting proxy.
//
// Example usage:
//
//	go build -o ~/bin/inverting-proxy ./server/server.go
//	~/bin/inverting-proxy --port 8081
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/inverting-proxy/agent/utils"
)

var (
	port         int
	setReadLimit int
	bufSize      int
	shimPath     string
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
	requestIDs    chan string
	randGenerator *rand.Rand

	// protects the map below
	sync.Mutex
	requests map[string]*pendingRequest
}

func newProxy() *proxy {
	return &proxy{
		requestIDs:    make(chan string),
		randGenerator: rand.New(rand.NewSource(time.Now().UnixNano())),
		requests:      make(map[string]*pendingRequest),
	}
}

type sessionMessage struct {
	ID          string      `json:"id,omitempty"`
	Message     interface{} `json:"msg,omitempty"`
	Version     int         `json:"v,omitempty"`
	Subprotocol string      `json:"s,omitempty"`
}

type messageData struct {
	data []byte
	mt   websocket.MessageType
}

type wsSessionHelper struct {
	sessionInfo sessionMessage
	writeChan   chan []byte
	readChan    chan []messageData
}

func newWsSessionHelper() *wsSessionHelper {
	return &wsSessionHelper{
		readChan:  make(chan []messageData),
		writeChan: make(chan []byte, bufSize),
	}
}

func (p *proxy) handleAgentPostResponse(w http.ResponseWriter, r *http.Request, requestID string) {
	p.Lock()
	pending, ok := p.requests[requestID]
	p.Unlock()
	if !ok {
		log.Printf("Could not find pending request: %q", requestID)
		http.NotFound(w, r)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(r.Body), pending.req)
	if err != nil {
		log.Printf("Could not parse response to request %q: %v", requestID, err)
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
		log.Printf("Could not read response to request %q: %v", requestID, err)
		http.Error(w, "Failure reading request body", http.StatusInternalServerError)
	}
}

func (p *proxy) handleAgentGetRequest(w http.ResponseWriter, r *http.Request, requestID string) {
	p.Lock()
	pending, ok := p.requests[requestID]
	p.Unlock()
	if !ok {
		log.Printf("Could not find pending request: %q", requestID)
		http.NotFound(w, r)
		return
	}
	log.Printf("Returning pending request: %q", requestID)
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
	log.Printf("Reporting pending requests: %s", respJSON)
	w.WriteHeader(http.StatusOK)
	w.Write(respJSON)
}

func (p *proxy) handleAgentRequest(w http.ResponseWriter, r *http.Request, backendID string) {
	requestID := r.Header.Get(utils.HeaderRequestID)
	if requestID == "" {
		log.Printf("Received new backend list request from %q", backendID)
		p.handleAgentListRequests(w, r)
		return
	}
	if r.Method == http.MethodPost {
		log.Printf("Received new backend post request from %q", backendID)
		p.handleAgentPostResponse(w, r, requestID)
		return
	}
	log.Printf("Received new backend get request from %q", backendID)
	p.handleAgentGetRequest(w, r, requestID)
}

func (p *proxy) newID() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d", p.randGenerator.Int63())))
	return fmt.Sprintf("%x", sum)
}

// isHopByHopHeader determines whether or not the given header name represents
// a header that is specific to a single network hop and thus should not be
// retransmitted by a proxy.
//
// See: https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers#hbh
func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func websocketShimResponseHandlerOpen(resp *http.Response, ws *wsSessionHelper) error {
	p, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%v/open: failed to read response from agent: %v", shimPath, err)
	}
	err = json.Unmarshal(p, &ws.sessionInfo)
	if err != nil {
		return fmt.Errorf("%v/open: failed to parse JSON encoded data: %v", shimPath, err)
	}
	return nil
}

func websocketShimResponseHandlerPoll(resp *http.Response, ws *wsSessionHelper) error {
	p, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response from agent: %v", err)
	}
	ws.writeChan <- p
	return nil
}

// if the request is a normal HTTP request and not related to the websocket shim, nil value
// can be passed into the wsSessionHelper parameter
func (p *proxy) handleFrontendRequest(w http.ResponseWriter, r *http.Request, ws *wsSessionHelper) error {
	id := p.newID()
	log.Printf("Received new frontend request %q", id)
	// Filter out hop-by-hop headers from the request
	for name := range r.Header {
		if isHopByHopHeader(name) {
			r.Header.Del(name)
		}
	}
	pending := newPendingRequest(r)
	p.Lock()
	p.requests[id] = pending
	p.Unlock()
	defer func() {
		p.Lock()
		delete(p.requests, id)
		p.Unlock()
	}()

	// Enqueue the request
	select {
	case <-r.Context().Done():
		// The client request was cancelled
		return fmt.Errorf("timeout waiting to enqueue the request ID for %q", id)
	case p.requestIDs <- id:
	}
	log.Printf("Request %q enqueued after %s", id, time.Since(pending.startTime))
	// Pull out and copy the response
	defer log.Printf("Response for %q received after %s", id, time.Since(pending.startTime))
	select {
	case <-r.Context().Done():
		// The client request was cancelled
		return fmt.Errorf("timeout waiting for the response to %q", id)
	case resp := <-pending.respChan:
		// websocket shim endpoint handling
		if ws != nil {
			if resp.StatusCode != http.StatusOK {
				respBody, err := io.ReadAll(resp.Body)
				if err != nil {
					return fmt.Errorf("%v: http status code is %v, error reading response body", r.URL.Path, resp.StatusCode)
				}
				return fmt.Errorf("%v: http status code %v, response: %v", r.URL.Path, resp.StatusCode, string(respBody))
			}
			switch r.URL.Path {
			case shimPath + "/open":
				return websocketShimResponseHandlerOpen(resp, ws)
			case shimPath + "/poll":
				return websocketShimResponseHandlerPoll(resp, ws)
			}
			return nil
		}

		// Copy all of the non-hop-by-hop headers to the proxied response
		for name, vals := range resp.Header {
			if isHopByHopHeader(name) {
				continue
			}
			w.Header()[name] = vals
		}
		w.Header().Add("transfer-encoding", "chunked")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		resp.Body.Close()
		for name, vals := range resp.Trailer {
			if isHopByHopHeader(name) {
				continue
			}
			for _, v := range vals {
				w.Header().Add(http.TrailerPrefix+name, v)
			}
		}
	}
	return nil
}

func parseServerMessage(buf interface{}) ([]byte, websocket.MessageType, error) {
	if data, ok := buf.(string); ok {
		return []byte(data), websocket.MessageText, nil
	}

	if arrData, ok := buf.([]interface{}); ok {
		if b64data, ok := arrData[0].(string); ok {
			data, err := base64.StdEncoding.DecodeString(b64data)
			if err == nil {
				return data, websocket.MessageBinary, nil
			}
		}
	}

	return nil, websocket.MessageBinary, errors.New("unexpected data format from server")
}

func newShimRequest(host string, endpoint string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("http://%v%v/%v", host, shimPath, endpoint),
		bytes.NewBuffer(body),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %v", err)
	}
	req.Header.Set("X-Websocket-Shim-Version", "1")
	return req, nil
}

func (p *proxy) handleWebsocketRequest(w http.ResponseWriter, r *http.Request) error {
	ws := newWsSessionHelper()

	// websocket: shimPath/open
	req, err := newShimRequest(r.Host, "open", []byte("ws://"+r.Host+r.URL.RequestURI()))
	if err != nil {
		return fmt.Errorf("failed to create a new open request: %v", err)
	}

	for k, v := range r.Header {
		if !isHopByHopHeader(k) {
			req.Header.Set(k, strings.Join(v, ", "))
		}
	}
	req.Header.Set("Origin", "") // to avoid CORS errors

	err = p.handleFrontendRequest(nil, req, ws)
	if err != nil {
		return err
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}

	background := context.Background()
	ctx, ctxCancel := context.WithCancel(background)

	// websocket: shimPath/close
	defer func() {
		buf, err := json.Marshal(sessionMessage{
			ID: ws.sessionInfo.ID,
		})
		if err != nil {
			log.Printf("Failed to encoded data in JSON format: %v", err)
			return
		}

		req, err := newShimRequest(r.Host, "close", buf)
		if err != nil {
			log.Printf("Failed to create a new close request: %v", err)
			return
		}
		p.handleFrontendRequest(nil, req, ws)
		ctxCancel()
		conn.CloseNow()
	}()

	if setReadLimit != 0 {
		conn.SetReadLimit(int64(setReadLimit))
	}

	// goroutine to read client messages
	go func() {
		for {
			mt, ior, err := conn.Reader(ctx)
			if err != nil {
				log.Printf("Failed to create websocket reader: %v", err)
				return
			}
			var bufArray []messageData
			for {
				buf := make([]byte, bufSize)
				bytesRead, err := ior.Read(buf)
				if bytesRead > 0 {
					bufArray = append(bufArray, messageData{buf[:bytesRead], mt})
				}
				if err != nil {
					break
				}
			}
			ws.readChan <- bufArray
		}
	}()

	// goroutine to read server messages
	go func() {
		for {
			payload := sessionMessage{
				ID: ws.sessionInfo.ID,
			}

			buf, err := json.Marshal(payload)
			if err != nil {
				log.Printf("Failed to encoded data in JSON format: %v", err)
				return
			}
			req, err := newShimRequest(r.Host, "poll", buf)
			if err != nil {
				log.Printf("Failed to create a new poll request: %v", err)
				return
			}
			err = p.handleFrontendRequest(nil, req, ws)
			if err != nil {
				log.Print(err)
				return
			}
		}
	}()

	// loop to process read/write events
	for {
		select {
		case <-ctx.Done():
			return errors.New("context closed")
		case msg := <-ws.readChan:
			var payload []sessionMessage
			for _, md := range msg {
				if md.mt == websocket.MessageText {
					payload = append(payload, sessionMessage{
						ID:          ws.sessionInfo.ID,
						Version:     ws.sessionInfo.Version,
						Message:     string(md.data),
						Subprotocol: ws.sessionInfo.Subprotocol,
					})
				} else {
					payload = append(payload, sessionMessage{
						ID:          ws.sessionInfo.ID,
						Version:     ws.sessionInfo.Version,
						Message:     []interface{}{md.data},
						Subprotocol: ws.sessionInfo.Subprotocol,
					})
				}
			}

			buf, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("failed to encoded data in JSON format: %v", err)
			}

			// send data to agent via HTTP
			req, err := newShimRequest(r.Host, "data", buf)
			if err != nil {
				return fmt.Errorf("failed to create a new data request: %v", err)
			}

			err = p.handleFrontendRequest(nil, req, ws)
			if err != nil {
				return err
			}
		case buf := <-ws.writeChan:
			var decodedBuf []interface{}
			err = json.Unmarshal(buf, &decodedBuf)
			if err != nil {
				return err
			}

			var msg []byte
			var msgType websocket.MessageType

			for i := range decodedBuf {
				msg, msgType, err = parseServerMessage(decodedBuf[i])
				if err != nil {
					return err
				}

				iow, err := conn.Writer(ctx, msgType)
				if err != nil {
					return err
				}

				bytesWritten, err := iow.Write(msg)
				if bytesWritten < len(msg) {
					if err != nil {
						return err
					} else {
						return errors.New("unexpected error while writing data from proxy to client")
					}
				}
				iow.Close()
			}
		}
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if backendID := r.Header.Get(utils.HeaderBackendID); backendID != "" {
		p.handleAgentRequest(w, r, backendID)
		return
	}

	if shimPath != "" && strings.ToLower(r.Header.Get("Connection")) == "upgrade" && strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
		err := p.handleWebsocketRequest(w, r)
		if err != nil {
			log.Print(err)
		}
		return
	}

	err := p.handleFrontendRequest(w, r, nil)
	if err != nil {
		log.Print(err)
	}
}

func main() {
	flag.IntVar(&port, "port", 0, "Port on which to listen")
	flag.IntVar(&setReadLimit, "ws-read-limit", -1, "websocket read limit from client in bytes")
	flag.IntVar(&bufSize, "ws-buffer-size", 1024*4, "websocket buffer size for writes")
	flag.StringVar(&shimPath, "shim-path", "", "Path under which to handle websocket shim requests")

	flag.Parse()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Failed to create the TCP listener for port %d: %v", port, err)
	}
	log.Printf("Listening on %s", listener.Addr())
	log.Fatal(http.Serve(listener, newProxy()))
}

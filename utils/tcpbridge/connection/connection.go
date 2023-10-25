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

// Package connection provides utilities for the connection used when bridging TCP over websocket.
package connection

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// StreamingPath is the path used for websocket channels that implement the bridge.
//
// The last component of the path is a randomly generated UUID used to reduce the
// risk of accidental conflicts.
const StreamingPath = "/tcp-over-websocket-bridge/35218cb7-1201-4940-89e8-48d8f03fed96"

// WebsocketNetConnection wraps the websocket.Conn type and adds extends it to implement the net.Conn interface
type WebsocketNetConn struct {
	*websocket.Conn
	bufferedMsg []byte
}

// SetDeadline implements the net.Conn interface.
func (c *WebsocketNetConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// Read implements the io.Reader interface.
func (c *WebsocketNetConn) Read(bs []byte) (count int, err error) {
	for len(c.bufferedMsg) == 0 {
		msgType, msg, err := c.ReadMessage()
		if err != nil {
			return 0, err
		}
		if msgType != websocket.TextMessage {
			continue
		}
		rawBytes, err := hex.DecodeString(string(msg))
		if err != nil {
			return 0, fmt.Errorf("failure decoding a websocket message: %w", err)
		}
		c.bufferedMsg = rawBytes
	}
	for count = 0; count < len(bs) && count < len(c.bufferedMsg); count++ {
		bs[count] = c.bufferedMsg[count]
	}
	c.bufferedMsg = c.bufferedMsg[count:]
	return count, nil
}

// Write implements the io.Writer interface.
func (c *WebsocketNetConn) Write(bs []byte) (count int, err error) {
	if err := c.WriteMessage(websocket.TextMessage, []byte(hex.EncodeToString(bs))); err != nil {
		return 0, fmt.Errorf("error writing a websocket message: %w", err)
	}
	return len(bs), nil
}

// DialWebsocket establishes a connection with the given server using websocket as the
// underlying transport layer.
func DialWebsocket(ctx context.Context, backendURL *url.URL, h http.Header) (net.Conn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, backendURL.String(), h)
	if err != nil {
		return nil, fmt.Errorf("unable to dial websocket: %w", err)
	}
	return &WebsocketNetConn{Conn: conn}, nil
}

// Handler returns an HTTP handler that forwards bridged TCP connections to the given port.
//
// The passthroughHandler is used to forward any requests that are not part of the TCP-Over-WS bridge.
func Handler(backendPort int, passthroughHandler http.Handler) http.Handler {
	backendHost := fmt.Sprintf("localhost:%d", backendPort)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) || r.URL.Path != StreamingPath {
			passthroughHandler.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		upgrader := websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		}
		r = r.WithContext(ctx)
		wsConn, err := upgrader.Upgrade(w, r, r.Header)
		if err != nil {
			log.Printf("Failure upgrading a websocket connection: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer wsConn.Close()
		conn := &WebsocketNetConn{Conn: wsConn}

		backendConn, err := net.Dial("tcp", backendHost)
		if err != nil {
			log.Printf("Failure establishing the backend connection: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			io.Copy(backendConn, conn)
		}()
		go func() {
			defer wg.Done()
			io.Copy(conn, backendConn)
		}()
		wg.Wait()
	})
}

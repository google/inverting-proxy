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

// Command backend implements the backend of a bridge that tunnels TCP over websocket.
//
// This allows arbitrary TCP protocols to be forwarded over the inverting proxy.
//
// To build, run:
//
//    $ make
//
// And to use, run:
//
//    $ $(GOPATH)/bin/tcp-over-ws-bridge-backend -port <PORT> -backend-port <BACKEND_PORT>
//
// The backend expects bridged requests to be sent over websocket connections to
// a fixed path.
//
// If it receives any requests that do not match those expectations, it will attempt
// to simply forward them exactly as-is to the backend port.

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"github.com/google/inverting-proxy/utils/tcpbridge/connection"
	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var (
	port        = flag.Int("port", 8080, "the port to serve on")
	backendPort = flag.Int("backend-port", 8081, "the TCP port to which to forward connections")
	forceHTTP2  = flag.Bool("force-http2", false, "if true, will force HTTP requests that are passed through to the backend unchanged to be sent over HTTP/2")
)

func main() {
	flag.Parse()
	backendHost := fmt.Sprintf("localhost:%d", *backendPort)
	passthroughBackend := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   backendHost,
	})
	passthroughBackend.FlushInterval = -1
	if *forceHTTP2 {
		passthroughBackend.Transport = &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network string, addr string, cfg *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		}
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) || r.URL.Path != connection.StreamingPath {
			passthroughBackend.ServeHTTP(w, r)
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
		conn := &connection.WebsocketNetConn{Conn: wsConn}

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
	h2h := h2c.NewHandler(handler, &http2.Server{})
	panic(http.ListenAndServe(fmt.Sprintf(":%d", *port), h2h))
}

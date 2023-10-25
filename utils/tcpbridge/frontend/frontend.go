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

// Command frontend implements the frontend of a bridge that tunnels TCP over websocket.
//
// This allows arbitrary TCP protocols to be forwarded over the inverting proxy.
//
// To build, run:
//
//    $ make
//
// And to use, run:
//
//    $ $(GOPATH)/bin/tcp-over-ws-bridge-frontend -frontend-port <PORT> -backend <BACKEND_URL>
//
// In order for this bridging to work, the backend must be running the `backend` binary
// provided in this same package (or another server implementing the same protocol).

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"path"
	"sync"

	"github.com/google/inverting-proxy/utils/tcpbridge/connection"
)

var (
	frontendPort = flag.Int("frontend-port", 8080, "the port to serve on")
	backend      = flag.String("backend", "", "URL of the backend server for the TCP-over-WS bridge")
)

func main() {
	flag.Parse()
	backendURL, err := url.Parse(*backend)
	if err != nil {
		log.Fatal(err)
	}
	backendURL.Path = path.Join(backendURL.Path, connection.StreamingPath)
	log.Printf("Backend URL: %+v", backendURL)
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *frontendPort))
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer conn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backendConn, err := connection.DialWebsocket(ctx, backendURL, nil)
			if err != nil {
				log.Printf("Failure dialing the backend connection: %v", err)
				return
			}
			defer backendConn.Close()

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
		}()
	}
}

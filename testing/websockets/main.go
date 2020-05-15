/*
Copyright 2019 Google Inc. All rights reserved.

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

// Command websockets launches a reverse proxy that can be used to
// manually test changes to the code in the agent/websockets package
//
// Example usage:
//   go build -o ~/bin/test-websocket-sever testing/websockets/main.go
//   ~/bin/test-websocket-server --port 8081 --backend http://localhost:8082
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/google/inverting-proxy/agent/websockets"
)

var (
	port     = flag.Int("port", 0, "Port on which to listen")
	backend  = flag.String("backend", "", "URL of the backend HTTP server to proxy")
	shimPath = flag.String("shim-path", "websocket-shim", "Path under which to handle websocket shim requests")
)

func main() {
	flag.Parse()

	if *backend == "" {
		log.Fatal("You must specify the address of the backend server to proxy")
	}
	if *port == 0 {
		log.Fatal("You must specify a local port number on which to listen")
	}
	backendURL, err := url.Parse(*backend)
	if err != nil {
		log.Fatalf("Failure parsing the address of the backend server: %v", err)
	}
	backendProxy := httputil.NewSingleHostReverseProxy(backendURL)
	shimmingProxy, err := websockets.Proxy(context.Background(), backendProxy, backendURL.Host, *shimPath, true, func(h http.Handler) http.Handler { return h })
	if err != nil {
		log.Fatal("Failure starting the websocket-shimming proxy: %v", err)
	}
	shimFunc, err := websockets.ShimBody(*shimPath)
	if err != nil {
		log.Fatal("Failure setting up shim injection code: %v", err)
	}
	backendProxy.ModifyResponse = shimFunc

	http.Handle("/", shimmingProxy)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

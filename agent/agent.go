/*
Copyright 2016 Google Inc. All rights reserved.

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

// Command agent forwards requests from an inverting proxy to a backend server.
//
// To build, run:
//
//    $ make
//
// And to use, run:
//
//    $ $(GOPATH)/bin/proxy-forwarding-agent -proxy <proxy-url> -backend <backend-ID>

package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/golang/groupcache/lru"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/google/inverting-proxy/agent/utils"
)

const (
	requestCacheLimit = 1000
	emailScope        = "email"

	maxBackoffDuration = 100 * time.Millisecond
)

var (
	proxy         = flag.String("proxy", "", "URL (including scheme) of the inverting proxy")
	host          = flag.String("host", "localhost:8080", "Hostname (including port) of the backend server")
	backendID     = flag.String("backend", "", "Unique ID for this backend.")
	debug         = flag.Bool("debug", false, "Whether or not to print debug log messages")
	forwardUserID = flag.Bool("forward-user-id", false, "Whether or not to include the ID (email address) of the end user in requests to the backend")
)

// forwardRequest forwards the given request from the proxy to
// the backend server and reports the response back to the proxy.
func forwardRequest(client *http.Client, request *utils.ForwardedRequest) error {
	httpRequest := request.Contents
	if *forwardUserID {
		httpRequest.Header.Add(utils.HeaderUserID, request.User)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   *host,
	})
	reverseProxy.FlushInterval = 100 * time.Millisecond
	responseForwarder, err := utils.NewResponseForwarder(client, *proxy, request.BackendID, request.RequestID)
	if err != nil {
		return err
	}
	reverseProxy.ServeHTTP(responseForwarder, httpRequest)
	if *debug {
		log.Printf("Backend latency for request %s: %s\n", request.RequestID, time.Since(request.StartTime).String())
	}
	return responseForwarder.Close()
}

// processOneRequest reads a single request from the proxy and forwards it to the backend server.
func processOneRequest(client *http.Client, backendID string, requestID string) {
	if err := utils.ReadRequest(client, *proxy, backendID, requestID, forwardRequest); err != nil {
		log.Printf("Failed to forward a request: [%s] %q\n", requestID, err.Error())
	}
}

func exponentialBackoffDuration(retryCount uint) time.Duration {
	targetDuration := (1 << retryCount) * time.Millisecond
	if targetDuration > maxBackoffDuration {
		return maxBackoffDuration
	}
	return targetDuration
}

// pollForNewRequests repeatedly reaches out to the proxy server to ask if any pending are available, and then
// processes any newly-seen ones.
func pollForNewRequests(client *http.Client, backendID string) {
	previouslySeenRequests := lru.New(requestCacheLimit)
	var retryCount uint
	for {
		if requests, err := utils.ListPendingRequests(client, *proxy, backendID); err != nil {
			log.Printf("Failed to read pending requests: %q\n", err.Error())
			time.Sleep(exponentialBackoffDuration(retryCount))
			retryCount += 1
		} else {
			retryCount = 0
			for _, requestID := range requests {
				if _, ok := previouslySeenRequests.Get(requestID); !ok {
					previouslySeenRequests.Add(requestID, requestID)
					go processOneRequest(client, backendID, requestID)
				}
			}
		}
	}
}

func getGoogleClient(ctx context.Context) (*http.Client, error) {
	sdkConfig, err := google.NewSDKConfig("")
	if err == nil {
		return sdkConfig.Client(ctx), nil
	}

	return google.DefaultClient(ctx, compute.CloudPlatformScope, emailScope)
}

// runAdapter sets up the HTTP client for the agent to use (including OAuth credentials),
// and then does the actual work of forwarding requests and responses.
func runAdapter() error {
	ctx := context.Background()

	client, err := getGoogleClient(ctx)
	if err != nil {
		return err
	}

	pollForNewRequests(client, *backendID)
	return nil
}

func main() {
	flag.Parse()

	if *proxy == "" {
		log.Fatal("You must specify the address of the proxy")
	}
	if *backendID == "" {
		log.Fatal("You must specify a backend ID")
	}

	if err := runAdapter(); err != nil {
		log.Fatal(err.Error())
	}
}

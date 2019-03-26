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
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/golang/groupcache/lru"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/google/inverting-proxy/agent/utils"
	"github.com/google/inverting-proxy/agent/websockets"
)

const (
	requestCacheLimit      = 1000
	emailScope             = "email"
	maxBackoffDuration     = 3 * time.Second
	firstRetryWaitDuration = time.Millisecond
)

var (
	proxy                = flag.String("proxy", "", "URL (including scheme) of the inverting proxy")
	proxyTimeout         = flag.Duration("proxy-timeout", 60*time.Second, "Client timeout when sending requests to the inverting proxy")
	host                 = flag.String("host", "localhost:8080", "Hostname (including port) of the backend server")
	backendID            = flag.String("backend", "", "Unique ID for this backend.")
	debug                = flag.Bool("debug", false, "Whether or not to print debug log messages")
	forwardUserID        = flag.Bool("forward-user-id", false, "Whether or not to include the ID (email address) of the end user in requests to the backend")
	shimWebsockets       = flag.Bool("shim-websockets", false, "Whether or not to replace websockets with a shim")
	shimPath             = flag.String("shim-path", "", "Path under which to handle websocket shim requests")
	healthCheckPath      = flag.String("health-check-path", "/", "Path on backend host to issue health checks against.  Defaults to the root.")
	healthCheckFreq      = flag.Int("health-check-interval-seconds", 0, "Wait time in seconds between health checks.  Set to zero to disable health checks.  Checks disabled by default.")
	healthCheckUnhealthy = flag.Int("health-check-unhealthy-threshold", 2, "A so-far healthy backend will be marked unhealthy after this many consecutive failures. The minimum value is 1.")
)

var (
	// compute the max retry count
	maxRetryCount = math.Log2(float64(maxBackoffDuration / firstRetryWaitDuration))
)

func hostProxy(ctx context.Context, host, shimPath string, injectShimCode bool) (http.Handler, error) {
	hostProxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   host,
	})
	hostProxy.FlushInterval = 100 * time.Millisecond
	if shimPath == "" {
		return hostProxy, nil
	}
	return websockets.Proxy(ctx, hostProxy, host, shimPath, injectShimCode)
}

// forwardRequest forwards the given request from the proxy to
// the backend server and reports the response back to the proxy.
func forwardRequest(client *http.Client, hostProxy http.Handler, request *utils.ForwardedRequest) error {
	httpRequest := request.Contents
	if *forwardUserID {
		httpRequest.Header.Add(utils.HeaderUserID, request.User)
	}
	responseForwarder, err := utils.NewResponseForwarder(client, *proxy, request.BackendID, request.RequestID)
	if err != nil {
		return fmt.Errorf("failed to create the response forwarder: %v", err)
	}
	hostProxy.ServeHTTP(responseForwarder, httpRequest)
	if *debug {
		log.Printf("Backend latency for request %s: %s\n", request.RequestID, time.Since(request.StartTime).String())
	}
	if err := responseForwarder.Close(); err != nil {
		return fmt.Errorf("failed to close the response forwarder: %v", err)
	}
	return nil
}

// healthCheck issues a health check against the backend server
// and returns the result.
func healthCheck() error {
	resp, err := http.Get("http://" + *host + *healthCheckPath)
	if err != nil {
		log.Printf("Health Check request failed: %s", err.Error())
		return err
	}
	if resp.StatusCode != 200 {
		log.Printf("Health Check request had non-200 status code: %d", resp.StatusCode)
		return fmt.Errorf("Bad Health Check Response Code: %s", resp.Status)
	}
	return nil
}

// processOneRequest reads a single request from the proxy and forwards it to the backend server.
func processOneRequest(client *http.Client, hostProxy http.Handler, backendID string, requestID string) {
	requestForwarder := func(client *http.Client, request *utils.ForwardedRequest) error {
		if err := forwardRequest(client, hostProxy, request); err != nil {
			log.Printf("Failure forwarding a request: [%s] %q\n", requestID, err.Error())
			return fmt.Errorf("failed to forward the request %q: %v", requestID, err)
		}
		return nil
	}
	if err := utils.ReadRequest(client, *proxy, backendID, requestID, requestForwarder); err != nil {
		log.Printf("Failed to forward a request: [%s] %q\n", requestID, err.Error())
	}
}

func exponentialBackoffDuration(retryCount uint) time.Duration {
	if retryCount > uint(maxRetryCount) {
		return maxBackoffDuration
	}

	targetDuration := (1 << retryCount) * firstRetryWaitDuration
	return targetDuration
}

// pollForNewRequests repeatedly reaches out to the proxy server to ask if any pending are available, and then
// processes any newly-seen ones.
func pollForNewRequests(client *http.Client, hostProxy http.Handler, backendID string) {
	previouslySeenRequests := lru.New(requestCacheLimit)
	var retryCount uint
	for {
		if requests, err := utils.ListPendingRequests(client, *proxy, backendID); err != nil {
			log.Printf("Failed to read pending requests: %q\n", err.Error())
			time.Sleep(exponentialBackoffDuration(retryCount))
			retryCount++
		} else {
			retryCount = 0
			for _, requestID := range requests {
				if _, ok := previouslySeenRequests.Get(requestID); !ok {
					previouslySeenRequests.Add(requestID, requestID)
					go processOneRequest(client, hostProxy, backendID, requestID)
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

	client, err := google.DefaultClient(ctx, compute.CloudPlatformScope, emailScope)
	if err != nil {
		return nil, err
	}
	client.Transport = utils.RoundTripperWithVMIdentity(ctx, client.Transport, *proxy)
	return client, nil
}

// waitForHealthy runs health checks against the backend and returns
// the first time it sees a healthy check.
func waitForHealthy() {
	if *healthCheckFreq <= 0 {
		return
	}
	if healthCheck() == nil {
		return
	}
	ticker := time.NewTicker(time.Duration(*healthCheckFreq) * time.Second)
	for _ = range ticker.C {
		if healthCheck() == nil {
			ticker.Stop()
			return
		}
	}
}

// runHealthChecks runs health checks against the backend and shuts down
// the proxy if the backend is unhealthy.
func runHealthChecks() {
	if *healthCheckFreq <= 0 {
		return
	}
	if *healthCheckUnhealthy < 1 {
		*healthCheckUnhealthy = 1
	}
	// Always start in the unhealthy state, but only require a single positive
	// health check to become healthy for the first time, and do the first check
	// immediately.
	ticker := time.NewTicker(time.Duration(*healthCheckFreq) * time.Second)
	badHealthChecks := 0
	for _ = range ticker.C {
		if healthCheck() != nil {
			badHealthChecks++
		} else {
			badHealthChecks = 0
		}
		if badHealthChecks >= *healthCheckUnhealthy {
			ticker.Stop()
			log.Fatal("Too many unhealthy checks")
		}
	}
}

// runAdapter sets up the HTTP client for the agent to use (including OAuth credentials),
// and then does the actual work of forwarding requests and responses.
func runAdapter() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := getGoogleClient(ctx)
	if err != nil {
		return err
	}
	client.Timeout = *proxyTimeout

	hostProxy, err := hostProxy(ctx, *host, *shimPath, *shimWebsockets)
	if err != nil {
		return err
	}
	pollForNewRequests(client, hostProxy, *backendID)
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
	if !strings.HasPrefix(*healthCheckPath, "/") {
		*healthCheckPath = "/" + *healthCheckPath
	}

	waitForHealthy()
	go runHealthChecks()

	if err := runAdapter(); err != nil {
		log.Fatal(err.Error())
	}
}

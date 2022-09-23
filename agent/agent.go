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
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
	compute "google.golang.org/api/compute/v1"
	"golang.org/x/oauth2/google"
	"github.com/golang/groupcache/lru"

	"github.com/google/inverting-proxy/agent/banner"
	"github.com/google/inverting-proxy/agent/metrics"
	"github.com/google/inverting-proxy/agent/sessions"
	"github.com/google/inverting-proxy/agent/utils"
	"github.com/google/inverting-proxy/agent/websockets"
)

const (
	requestCacheLimit   = 1000
	emailScope          = "email"
	headerAuthorization = "Authorization"
)

var (
	proxy                     = flag.String("proxy", "", "URL (including scheme) of the inverting proxy")
	proxyTimeout              = flag.Duration("proxy-timeout", 60*time.Second, "Client timeout when sending requests to the inverting proxy")
	host                      = flag.String("host", "localhost:8080", "Hostname (including port) of the backend server")
	backendID                 = flag.String("backend", "", "Unique ID for this backend.")
	debug                     = flag.Bool("debug", false, "Whether or not to print debug log messages")
	disableSSLForTest         = flag.Bool("disable-ssl-for-test", false, "Disable requirements for SSL when running tests locally")
	favIconURL                = flag.String("favicon-url", "", "URL of the favicon")
	forwardUserID             = flag.Bool("forward-user-id", false, "Whether or not to include the ID (email address) of the end user in requests to the backend")
	injectBanner              = flag.String("inject-banner", "", "HTML snippet to inject in served webpages")
	bannerHeight              = flag.String("banner-height", "40px", "Height of the injected banner. This is ignored if no banner is set.")
	shimWebsockets            = flag.Bool("shim-websockets", false, "Whether or not to replace websockets with a shim")
	shimPath                  = flag.String("shim-path", "", "Path under which to handle websocket shim requests")
	healthCheckPath           = flag.String("health-check-path", "/", "Path on backend host to issue health checks against.  Defaults to the root.")
	healthCheckFreq           = flag.Int("health-check-interval-seconds", 0, "Wait time in seconds between health checks.  Set to zero to disable health checks.  Checks disabled by default.")
	healthCheckUnhealthy      = flag.Int("health-check-unhealthy-threshold", 2, "A so-far healthy backend will be marked unhealthy after this many consecutive failures. The minimum value is 1.")
	disableGCEVM              = flag.Bool("disable-gce-vm-header", false, "Disable the agent from adding a GCE VM header.")
	enableWebsocketsInjection = flag.Bool("enable-websockets-injection", false, "Enables the injection of HTTP headers into websocket messages. "+
		"Websocket message injection will inject all headers from the HTTP request to /data and inject them "+
		"into JSON-serialized websocket messages at the JSONPath `resource.headers`")

	sessionCookieName       = flag.String("session-cookie-name", "", "Name of the session cookie; an empty value disables agent-based session tracking")
	sessionCookieTimeout    = flag.Duration("session-cookie-timeout", 12*time.Hour, "Expiration flag for the session cookie")
	sessionCookieCacheLimit = flag.Int("session-cookie-cache-limit", 1000, "Upper bound on the number of concurrent sessions that can be tracked by the agent")
	rewriteWebsocketHost    = flag.Bool("rewrite-websocket-host", false, "Whether to rewrite the Host header to the original request when shimming a websocket connection")
	stripCredentials        = flag.Bool("strip-credentials", false, "Whether to strip the Authorization header from all requests.")

	projectID                = flag.String("monitoring-project-id", "", "Name of the GCP project id")
	metricDomain             = flag.String("metric-domain", "", "Domain under which to write metrics eg. notebooks.googleapis.com")
	monitoringEndpoint       = flag.String("monitoring-endpoint", "monitoring.googleapis.com:443", "The endpoint to which to write metrics. Eg: monitoring.googleapis.com corresponds to Cloud Monarch")
	monitoringResourceType   = flag.String("monitoring-resource-type", "gce_instance", "The monitoring resource type. Eg: gce_instance")
	monitoringResourceLabels = flag.String("monitoring-resource-labels", "", "Comma separated key value pairs specifying the resource labels. Eg: 'instance-id=12345678901234,instance-zone=us-west1-a")
	gracefulShutdownTimeout  = flag.Duration("graceful-shutdown-timeout", 0, "Timeout for graceful shutdown. Enabled only if greater than 0.")

	sessionLRU    *sessions.Cache
	metricHandler *metrics.MetricHandler
)

func hostProxy(ctx context.Context, host, shimPath string, injectShimCode bool) (http.Handler, error) {
	hostProxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   host,
	})
	hostProxy.FlushInterval = 100 * time.Millisecond
	var h http.Handler = hostProxy
	h = sessionLRU.SessionHandler(h, metricHandler)
	if shimPath != "" {
		var err error
		// Note that we pass in the sessionHandler to the websocket proxy twice (h and sessionLRU.SessionHandler)
		// h is the wrapped handler that will handle all non-websocket-shim requests
		// sessionLRU.SessionHandler will be called for websocket open requests.
		// This is necessary to handle an edge case in session handling. Websocket open requests arrive with a path
		// of `/$shimPath/open`. The original target URL and path are encoded in the request body, which is
		// restored in the websocket handler. This means that attempting to restore session cookies that are
		// restricted to a path prefix not equal to "/" will fail for websocket open requests. Passing in the
		// sessionHandler twice allows the websocket handler to ensure that cookies are applied based on the
		// correct, restored path.
		h, err = websockets.Proxy(ctx, h, host, shimPath, *rewriteWebsocketHost, *enableWebsocketsInjection, sessionLRU.SessionHandler, metricHandler)
		if injectShimCode {
			shimFunc, err := websockets.ShimBody(shimPath)
			if err != nil {
				return nil, err
			}
			hostProxy.ModifyResponse = shimFunc
		}
		if err != nil {
			return nil, err
		}
	}
	if *injectBanner == "" {
		return h, nil
	}
	return banner.Proxy(ctx, h, *injectBanner, *bannerHeight, *favIconURL, metricHandler)
}

// forwardRequest forwards the given request from the proxy to
// the backend server and reports the response back to the proxy.
func forwardRequest(client *http.Client, hostProxy http.Handler, request *utils.ForwardedRequest) error {
	httpRequest := request.Contents
	if *debug {
		log.Printf("Request %s: %s %s %v\n", request.RequestID, request.Contents.Method, request.Contents.Host, request.Contents.ContentLength)
	}
	if *forwardUserID {
		httpRequest.Header.Add(utils.HeaderUserID, request.User)
	}
	if *stripCredentials {
		httpRequest.Header.Del(headerAuthorization)
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
	defer resp.Body.Close()
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
	if err := utils.ReadRequest(client, *proxy, backendID, requestID, requestForwarder, metricHandler); err != nil {
		log.Printf("Failed to forward a request: [%s] %q\n", requestID, err.Error())
	}
}

// pollForNewRequests repeatedly reaches out to the proxy server to ask if any pending are available, and then
// processes any newly-seen ones.
func pollForNewRequests(pollingCtx context.Context, client *http.Client, hostProxy http.Handler, backendID string) {
	previouslySeenRequests := lru.New(requestCacheLimit)

	var retryCount uint
	for {
		select {
		case <-pollingCtx.Done():
			log.Printf("Request polling context completed with ctx err: %v\n", pollingCtx.Err())
			return
		default:
			if requests, err := utils.ListPendingRequests(client, *proxy, backendID, metricHandler); err != nil {
				log.Printf("Failed to read pending requests: %q\n", err.Error())
				time.Sleep(utils.ExponentialBackoffDuration(retryCount))
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
	client.Transport = utils.RoundTripperWithVMIdentity(ctx, client.Transport, *proxy, *disableGCEVM)
	client.Jar, err = cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		return nil, err
	}
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
	for range ticker.C {
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
	for range ticker.C {
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
func runAdapter(ctx context.Context, requestPollingCtx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
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
	pollForNewRequests(requestPollingCtx, client, hostProxy, *backendID)
	return nil
}

func main() {
	flag.Parse()
	ctx := context.Background()

	if *proxy == "" {
		log.Fatal("You must specify the address of the proxy")
	}
	if *backendID == "" {
		log.Fatal("You must specify a backend ID")
	}
	if !strings.HasPrefix(*healthCheckPath, "/") {
		*healthCheckPath = "/" + *healthCheckPath
	}
	if *sessionCookieName != "" {
		sessionLRU = sessions.NewCache(*sessionCookieName, *sessionCookieTimeout, *sessionCookieCacheLimit, *disableSSLForTest)
	}
	mh, err := metrics.NewMetricHandler(ctx, *projectID, *monitoringResourceType, *monitoringResourceLabels, *metricDomain, *monitoringEndpoint)
	metricHandler = mh
	if err != nil {
		log.Printf("Unable to create metric handler: %v", err)
	}

	waitForHealthy()
	go runHealthChecks()

	requestPollingCtx, requestPollingCancel := context.WithCancel(ctx)

	go func(ctx context.Context) {
		if err := runAdapter(ctx, requestPollingCtx); err != nil {
			log.Fatal(err.Error())
		}
	}(ctx)

	osShutdownSignalCh := utils.ShutdownSignalChan()
	<-osShutdownSignalCh

	if *gracefulShutdownTimeout > 0 {
		requestPollingCancel()
		log.Printf("Begin graceful shutdown. Wait for: %v\n", *gracefulShutdownTimeout)
		time.Sleep(*gracefulShutdownTimeout)
		log.Fatal("Graceful shutdown wait timer end. Shutting down server.")
	}
}

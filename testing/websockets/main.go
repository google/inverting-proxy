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
//
//	go build -o ~/bin/test-websocket-server testing/websockets/main.go
//	~/bin/test-websocket-server --port 8081 --backend http://localhost:8082
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/google/inverting-proxy/agent/metrics"
	"github.com/google/inverting-proxy/agent/websockets"
)

var (
	port                     = flag.Int("port", 0, "Port on which to listen")
	backend                  = flag.String("backend", "", "URL of the backend HTTP server to proxy")
	shimPath                 = flag.String("shim-path", "websocket-shim", "Path under which to handle websocket shim requests")
	enableWebsocketInjection = flag.Bool("enable-websocket-injection", false, "Enables websockets message injection. "+
		"Websocket message injection will inject all headers from the HTTP request to /data and inject them "+
		"into JSON-serialized websocket messages at the JSONPath `resource.headers`")
	projectID                = flag.String("monitoring-project-id", "", "Name of the GCP project id")
	metricDomain             = flag.String("metric-domain", "", "Domain under which to write metrics eg. notebooks.googleapis.com")
	monitoringEndpoint       = flag.String("monitoring-endpoint", "staging-monitoring.sandbox.googleapis.com:443", "The endpoint to which to write metrics. Eg: monitoring.googleapis.com corresponds to Cloud Monarch.")
	monitoringResourceType   = flag.String("monitoring-resource-type", "gce_instance", "The monitoring resource type. Eg: gce_instance")
	monitoringResourceLabels = flag.String("monitoring-resource-labels", "instance-id=fake-instance-id,instance-zone=us-west1-a", "Comma separated key value pairs for the purpose of monitoring configuration. Eg: 'instance-id=my-instance-id,instance-zone=us-west1-a")
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
	metricHandler, err := metrics.NewMetricHandler(context.Background(), *projectID, *monitoringResourceType, *monitoringResourceLabels, *metricDomain, *monitoringEndpoint)
	if err != nil {
		log.Printf("Unable to create metric handler: %v", err)
	}

	backendProxy := httputil.NewSingleHostReverseProxy(backendURL)
	shimmingProxy, err := websockets.Proxy(context.Background(), backendProxy, backendURL.Host, *shimPath, true, *enableWebsocketInjection, func(h http.Handler, metricHandler *metrics.MetricHandler) http.Handler { return h }, metricHandler)
	if err != nil {
		log.Fatalf("Failure starting the websocket-shimming proxy: %v", err)
	}
	shimFunc, err := websockets.ShimBody(*shimPath)
	if err != nil {
		log.Fatalf("Failure setting up shim injection code: %v", err)
	}
	backendProxy.ModifyResponse = shimFunc

	http.Handle("/", shimmingProxy)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

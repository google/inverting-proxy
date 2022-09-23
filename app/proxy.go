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

// Package proxy defines an App Engine app implementing an inverse proxy.
//
// To deploy, export the ID of your project as ${PROJECT_ID} and then run:
//
//	$ make deploy
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/memcache"
	"google.golang.org/appengine/user"
)

const (
	requestsWaitTimeout = 30 * time.Second
	responseWaitTimeout = 30 * time.Second

	// HeaderUserID is the HTTP response header used to report the end user.
	HeaderUserID = "X-Inverting-Proxy-User-ID"

	// HeaderRequestID is the HTTP request/response header used to uniquely identify an end-user request.
	HeaderRequestID = "X-Inverting-Proxy-Request-ID"

	// HeaderBackendID is the HTTP request/response header used to uniquely identify a proxied backend server.
	HeaderBackendID = "X-Inverting-Proxy-Backend-ID"

	// HeaderRequestStartTime is the HTTP response header used to report when an end-user request was initiated.
	HeaderRequestStartTime = "X-Inverting-Proxy-Request-Start-Time"
)

func waitForNextRequests(ctx context.Context, s Store, backendID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestsWaitTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("Timeout waiting for requests for %q", backendID)
		default:
			if rs, err := s.ListPendingRequests(ctx, backendID); err == nil {
				if len(rs) > 0 {
					return rs, nil
				}
			} else {
				log.Warningf(ctx, "Error listing pending requests: %v", err)
			}
			// Wait a bit before checking again
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func waitForResponse(ctx context.Context, s Store, backendID, requestID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, responseWaitTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("Timeout waiting for a response from %q", backendID)
		default:
			if r, err := s.ReadResponse(ctx, backendID, requestID); err == nil {
				if len(r.Contents) > 0 {
					return r.Contents, nil
				}
			} else if err != datastore.ErrNoSuchEntity {
				log.Warningf(ctx, "Error waiting for a response: %v", err)
			}
			// Wait a bit before checking again
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func postRequest(ctx context.Context, s Store, backendID, requestID, userEmail string, requestBytes []byte) (*Request, error) {
	request := NewRequest(backendID, requestID, userEmail, requestBytes)
	if err := s.WriteRequest(ctx, request); err != nil {
		return nil, err
	}
	return request, nil
}

func parseResponse(backendID string, r *http.Request) (*Response, error) {
	requestID := r.Header.Get(HeaderRequestID)
	if requestID == "" {
		return nil, errors.New("No request ID specified")
	}
	responseBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return &Response{
		BackendID: backendID,
		RequestID: requestID,
		Contents:  responseBytes,
	}, nil
}

func postResponse(ctx context.Context, s Store, response *Response, errChan chan error) {
	backendID := response.BackendID
	requestID := response.RequestID
	log.Infof(ctx, "Responding to request [%q] for the backend %q", requestID, backendID)
	request, err := s.ReadRequest(ctx, backendID, requestID)
	if err != nil {
		errChan <- fmt.Errorf("No matching request: %q", requestID)
		return
	}
	response.StartTime = request.StartTime
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.WriteResponse(ctx, response); err != nil {
			errChan <- err
		}
	}()
	go func() {
		defer wg.Done()
		request.Completed = true
		if err := s.WriteRequest(ctx, request); err != nil {
			errChan <- err
		}
	}()
	wg.Wait()
}

func checkBackendID(ctx context.Context, s Store, r *http.Request) (string, error) {
	oauthUser, err := user.CurrentOAuth(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("Failed to read the OAuth authorization header: %q", err.Error())
	}
	backendUser := oauthUser.Email
	backendID := r.Header.Get(HeaderBackendID)
	if backendID == "" {
		return "", errors.New("No backend ID specified")
	}

	allowed, err := s.IsBackendUserAllowed(ctx, backendUser, backendID)
	if err != nil {
		return "", fmt.Errorf("Failed to look up backend user: %q", err.Error())
	}
	if !allowed {
		return "", errors.New("Backend user not allowed")
	}
	return backendID, nil
}

func pendingHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	backendID, err := checkBackendID(ctx, s, r)
	if err != nil {
		log.Errorf(ctx, "Failed to validate backend ID: %q", err.Error())
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	pendingRequests, err := waitForNextRequests(ctx, s, backendID)
	if err != nil {
		log.Infof(ctx, "Found no new requests for %q: %q", backendID, err.Error())
		pendingRequests = []string{}
	}

	requestsBytes, err := json.Marshal(pendingRequests)
	if err != nil {
		log.Errorf(ctx, "Failed to serialize the requests for %q: %q", backendID, err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(requestsBytes)
}

func requestHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	backendID, err := checkBackendID(ctx, s, r)
	if err != nil {
		log.Errorf(ctx, "Failed to validate backend ID: %q", err.Error())
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	requestID := r.Header.Get(HeaderRequestID)
	if requestID == "" {
		errorMsg := "No request ID specified"
		log.Errorf(ctx, errorMsg)
		http.Error(w, errorMsg, http.StatusBadRequest)
		return
	}
	w.Header().Add(HeaderRequestID, requestID)

	request, err := s.ReadRequest(ctx, backendID, requestID)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to read the request for %q: %q", requestID, err.Error())
		log.Errorf(ctx, errorMsg)
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}
	w.Header().Add(HeaderUserID, request.User)

	startTimeText := request.StartTime.Format(time.RFC3339Nano)
	w.Header().Add(HeaderRequestStartTime, startTimeText)
	w.Write(request.Contents)
}

func responseHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	backendID, err := checkBackendID(ctx, s, r)
	if err != nil {
		log.Errorf(ctx, "Failed to validate backend ID: %q", err.Error())
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	response, err := parseResponse(backendID, r)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to read the response body: %q", err.Error())
		log.Errorf(ctx, errorMsg)
		http.Error(w, errorMsg, http.StatusBadRequest)
		return
	}
	notFoundErrs := make(chan error, 1)
	log.Infof(ctx, "Posting a response [%q]", response.RequestID)
	postResponse(ctx, s, response, notFoundErrs)
	close(notFoundErrs)
	if len(notFoundErrs) > 0 {
		notFoundErr := <-notFoundErrs
		log.Errorf(ctx, notFoundErr.Error())
		http.Error(w, notFoundErr.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func deleteHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	log.Infof(ctx, "Deleting old data")
	if err := s.DeleteOldBackends(ctx); err != nil {
		log.Errorf(ctx, "Failed to delete old backend data: %q", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.DeleteOldRequests(ctx); err != nil {
		log.Errorf(ctx, "Failed to delete old request data: %q", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write([]byte{})
	return
}

func reportError(w http.ResponseWriter, requestID string, err error, statusCode int) {
	w.Header().Add(HeaderRequestID, requestID)
	http.Error(w, err.Error(), statusCode)
}

func forwardResponse(ctx context.Context, requestID string, w http.ResponseWriter, response *http.Response) {
	for k, v := range response.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		log.Errorf(ctx, "Failed to copy a response: %v", err)
	}
}

func cacheResponse(ctx context.Context, cacheKey string, responseBytes []byte) error {
	cacheItem := &memcache.Item{
		Key:   cacheKey,
		Value: responseBytes,
	}
	return memcache.Set(ctx, cacheItem)
}

func readCachedResponse(ctx context.Context, cacheKey string, r *http.Request) (*http.Response, error) {
	cacheItem, err := memcache.Get(ctx, cacheKey)
	if err != nil {
		return nil, err
	}
	responseReader := bufio.NewReader(bytes.NewReader(cacheItem.Value))
	return http.ReadResponse(responseReader, r)
}

func proxyHandler(ctx context.Context, s Store, requestID string, w http.ResponseWriter, r *http.Request) {
	currentUser := user.Current(ctx)
	if currentUser == nil {
		http.Error(w, "You must be signed in to App Engine", http.StatusUnauthorized)
		return
	}
	userEmail := currentUser.Email
	backendID, err := s.LookupBackend(ctx, userEmail, r.URL.Path)
	if err != nil {
		log.Infof(ctx, "No matching backends for %q", userEmail)
		http.NotFound(w, r)
		return
	}

	cacheKey := fmt.Sprintf("cache:%q:%q", userEmail, r.URL.String())
	if r.Method == http.MethodGet {
		if response, err := readCachedResponse(ctx, cacheKey, r); err == nil {
			forwardResponse(ctx, requestID, w, response)
			return
		}
	}

	// Note that App Engine has a safety limit on request sizes of 32MB
	// (see https://cloud.google.com/appengine/quotas#Requests), so
	// the following buffer cannot grow larger than that.
	var requestBuffer bytes.Buffer
	if err := r.Write(&requestBuffer); err != nil {
		log.Errorf(ctx, "Failed to serialize request: %q", err.Error())
		reportError(w, requestID, err, http.StatusInternalServerError)
		return
	}

	requestBytes, err := ioutil.ReadAll(&requestBuffer)
	if err != nil {
		log.Errorf(ctx, "Failed to serialize the body for request %q", err.Error())
		reportError(w, requestID, err, http.StatusInternalServerError)
		return
	}

	if _, err := postRequest(ctx, s, backendID, requestID, userEmail, requestBytes); err != nil {
		log.Errorf(ctx, "Failed to cache a new request request %q", err.Error())
		reportError(w, requestID, err, http.StatusInternalServerError)
		return
	}

	responseBytes, err := waitForResponse(ctx, s, backendID, requestID)
	if err != nil {
		log.Errorf(ctx, "Failed to receive the response for request %q: %q", requestID, err.Error())
		reportError(w, requestID, err, http.StatusGatewayTimeout)
		return
	}
	responseReader := bufio.NewReader(bytes.NewReader(responseBytes))
	response, err := http.ReadResponse(responseReader, r)
	if err != nil {
		log.Errorf(ctx, "Failed to read the response for request %q: %q", requestID, err.Error())
		reportError(w, requestID, err, http.StatusInternalServerError)
		return
	}
	if r.Method == "GET" && response.StatusCode == 200 && response.Header.Get("Cache-Control") == "" {
		if err := cacheResponse(ctx, cacheKey, responseBytes); err != nil {
			log.Errorf(ctx, "Failed to cache response: %q", err.Error())
		}
	}
	forwardResponse(ctx, requestID, w, response)
}

func parseBackend(r *http.Request) (*Backend, error) {
	requestBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	var backend Backend
	if err := json.Unmarshal(requestBytes, &backend); err != nil {
		return nil, err
	}

	if backend.BackendID == "" || backend.BackendUser == "" || backend.EndUser == "" || len(backend.PathPrefixes) == 0 {
		return nil, errors.New("Missing required fields")
	}

	return &backend, nil
}

func addBackendHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	backend, err := parseBackend(r)
	if err != nil {
		log.Errorf(ctx, "Failed to parse the backend: %q", err.Error())
		http.Error(w, "Failed to parse the backend definition", http.StatusBadRequest)
		return
	}
	if err := s.AddBackend(ctx, backend); err != nil {
		log.Errorf(ctx, "Failed to add a new backend: %q", err.Error())
		http.Error(w, "Failed to add a backend", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func listBackendsHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	backends, err := s.ListBackends(ctx)
	if err != nil {
		log.Errorf(ctx, "Failed to list backends: %q", err.Error())
		http.Error(w, "Failed to list backends", http.StatusInternalServerError)
		return
	}

	responseBytes, err := json.Marshal(backends)
	if err != nil {
		log.Errorf(ctx, "Failed to serialize the backends: %q", err.Error())
		http.Error(w, "Failed to list backends", http.StatusInternalServerError)
		return
	}

	w.Write(responseBytes)
}

func deleteBackendHandler(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	backendID := strings.TrimPrefix(r.URL.Path, "/api/backends/")
	if backendID == "" {
		http.Error(w, "No backend specified", http.StatusBadRequest)
		return
	}

	if err := s.DeleteBackend(ctx, backendID); err != nil {
		log.Errorf(ctx, "Failed to delete the backend %q: %q", backendID, err.Error())
		http.Error(w, "Failed to delete the backend", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// isAgentRequest checks if the given request is a forwarding request from an agent.
func isAgentRequest(ctx context.Context) bool {
	return appengine.ModuleName(ctx) == "agent"
}

// isAPIRequest checks if the given request is an API request.
func isAPIRequest(ctx context.Context) bool {
	return appengine.ModuleName(ctx) == "api"
}

// handleAgentRequest routes a request from a forwarding agent to the appropriate request handler.
func handleAgentRequest(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/agent/pending") {
		pendingHandler(ctx, s, w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/agent/request") {
		requestHandler(ctx, s, w, r)
		return
	} else if strings.HasPrefix(r.URL.Path, "/agent/response") {
		responseHandler(ctx, s, w, r)
		return
	}
	log.Errorf(ctx, "Invalid agent path: %q", r.URL.Path)
	http.Error(w, "Invalid request path", http.StatusNotFound)
	return
}

// isAdminRequest checks if the current request was issued by an app administrator.
//
// This is different from `user.IsAdmin(ctx)`, as that method does not work for
// requests that are authenticated using OAuth.
func isAdminRequest(ctx context.Context) bool {
	if user.IsAdmin(ctx) {
		return true
	}
	oauthUser, err := user.CurrentOAuth(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		log.Infof(ctx, "Failed to read the OAuth authorization header for an API call: %q", err.Error())
		return false
	}
	return oauthUser.Admin
}

// handleAPIRequest routes an API request from an app administrator to the appropriate request handler.
func handleAPIRequest(ctx context.Context, s Store, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/cron/delete" {
		// We don't check the user identity here as this path is restricted to admin users.
		deleteHandler(ctx, s, w, r)
		return
	}

	log.Infof(ctx, "Received an admin API request: %q", r.URL.Path)
	if !isAdminRequest(ctx) {
		log.Infof(ctx, "Received an admin API request from a non-admin user")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if r.URL.Path == "/api/backends" {
		if r.Method == http.MethodGet {
			listBackendsHandler(ctx, s, w, r)
			return
		} else if r.Method == http.MethodPost {
			addBackendHandler(ctx, s, w, r)
			return
		}
		log.Errorf(ctx, "Unsupported API method for %q: %q", r.URL.Path, r.Method)
		http.Error(w, "Unsupported API method: "+r.Method, http.StatusMethodNotAllowed)
		return
	} else if strings.HasPrefix(r.URL.Path, "/api/backends/") {
		if r.Method == http.MethodDelete {
			deleteBackendHandler(ctx, s, w, r)
			return
		}
		log.Errorf(ctx, "Unsupported API method for %q: %q", r.URL.Path, r.Method)
		http.Error(w, "Unsupported API method: "+r.Method, http.StatusMethodNotAllowed)
		return
	}

	log.Errorf(ctx, "Invalid API path: %q", r.URL.Path)
	http.Error(w, "Invalid request path", http.StatusNotFound)
	return
}

func init() {
	s := NewCachingStore(NewPersistentStore())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := appengine.NewContext(r)
		if isAgentRequest(ctx) {
			handleAgentRequest(ctx, s, w, r)
			return
		} else if isAPIRequest(ctx) {
			handleAPIRequest(ctx, s, w, r)
			return
		}
		ID := appengine.RequestID(ctx)
		proxyHandler(ctx, s, ID, w, r)
	})
}

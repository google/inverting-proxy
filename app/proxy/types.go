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

package proxy

import (
	"context"
	"time"
)

// Request represents an end-user request that we forward.
type Request struct {
	BackendID string
	RequestID string
	User      string
	Contents  []byte
	StartTime time.Time
	Completed bool
}

// Response represents the response to an end-user request that we forward.
type Response struct {
	BackendID string
	RequestID string
	Contents  []byte
	StartTime time.Time
}

// Backend represents a backend server that is allowed to process requests.
type Backend struct {
	// BackendID is a string that uniquely identifies a specific backend instance.
	BackendID string `json:"id,omitempty"`

	// BackendUser is the email address of the account the backend agent uses to authenticate.
	BackendUser string `json:"backendUser,omitempty"`

	// EndUser is the email address of the end user for whom requests should be
	// routed to this backend. To include all users use the literal string 'allUsers'.
	EndUser string `json:"endUser,omitempty"`

	// PathPrefixes are the list of path prefixes for which requests should
	// be routed to this backend.
	PathPrefixes []string `json:"pathPrefixes,omitempty"`

	// LastUsed encodes the timestamp for the last request that was received
	// for the backend. This is an output-only field and is automatically
	// populated based on the recorded requests.
	LastUsed string `json:"lastUsed,omitempty"`
}

// NewRequest converts the given request information to our internal representation.
func NewRequest(backendID, requestID, userEmail string, requestBytes []byte) *Request {
	return &Request{
		BackendID: backendID,
		RequestID: requestID,
		User:      userEmail,
		Contents:  requestBytes,
		StartTime: time.Now(),
	}
}

// Store defines the interface for reading and writing our internal metadata
// about requests, responses, and backends.
type Store interface {
	// WriteRequest writes the given request to the Store.
	WriteRequest(ctx context.Context, r *Request) error

	// ReadRequest reads the specified request from the Store.
	ReadRequest(ctx context.Context, backendID, requestID string) (*Request, error)

	// ListPendingRequests returns the list of request IDs for the given backend
	// that do not yet have a stored response.
	ListPendingRequests(ctx context.Context, backendID string) ([]string, error)

	// WriteResponse writes the given response to the Store.
	WriteResponse(ctx context.Context, r *Response) error

	// ReadResponse reads from the Store the response to the specified request.
	//
	// If there is no response yet, then the returned value is nil.
	ReadResponse(ctx context.Context, backendID, requestID string) (*Response, error)

	// DeleteOldRequests deletes from the Store any backend records older than some threshold.
	DeleteOldBackends(ctx context.Context) error

	// DeleteOldRequests deletes from the Store any requests older than some threshold.
	DeleteOldRequests(ctx context.Context) error

	// AddBackend adds the definition of the backend to the store
	AddBackend(ctx context.Context, backend *Backend) error

	// ListBackends lists all of the backends.
	ListBackends(ctx context.Context) ([]*Backend, error)

	// DeleteBackend deletes the given backend.
	DeleteBackend(ctx context.Context, backendID string) error

	// LookupBackend looks up the backend for the given user and path.
	LookupBackend(ctx context.Context, endUser, path string) (string, error)

	// IsBackendUserAllowed checks whether the given user can act as the specified backend.
	IsBackendUserAllowed(ctx context.Context, backendUser, backendID string) (bool, error)
}

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
	"fmt"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/appengine/memcache"
)

const (
	cacheEntrySizeLimit = 1000000
)

type cachingStore struct {
	BackingStore Store
}

// NewCachingStore returns a new implementation of the Store interface that uses the
// App Engine Memcache service to improve performance.
//
// The passed in Store is used as the authoritative source of truth to check
// when a request cannot be served solely out of the cache.
func NewCachingStore(s Store) Store {
	return &cachingStore{
		BackingStore: s,
	}
}

type cachedBackend struct {
	LastUsed time.Time
}

func memcacheRequestKey(backendID, requestID string) string {
	return fmt.Sprintf("r:%q:%q", backendID, requestID)
}

// WriteRequest writes the given request to the Store.
func (c *cachingStore) WriteRequest(ctx context.Context, r *Request) error {
	if len(r.Contents) < cacheEntrySizeLimit {
		memcache.Gob.Set(ctx, &memcache.Item{
			Key:    memcacheRequestKey(r.BackendID, r.RequestID),
			Object: r,
		})
	}
	return c.BackingStore.WriteRequest(ctx, r)
}

// ReadRequest reads the specified request from the Store.
func (c *cachingStore) ReadRequest(ctx context.Context, backendID string, requestID string) (*Request, error) {
	var r Request
	if _, err := memcache.Gob.Get(ctx, memcacheRequestKey(backendID, requestID), &r); err == nil {
		return &r, nil
	}
	return c.BackingStore.ReadRequest(ctx, backendID, requestID)
}

// ListPendingRequests returns the list or requests for the given backend
// that do not yet have a stored response.
func (c *cachingStore) ListPendingRequests(ctx context.Context, backendID string) ([]string, error) {
	return c.BackingStore.ListPendingRequests(ctx, backendID)
}

func memcacheResponseKey(backendID, requestID string) string {
	return fmt.Sprintf("resp:%q:%q", backendID, requestID)
}

// WriteResponse writes the given response to the Store.
func (c *cachingStore) WriteResponse(ctx context.Context, r *Response) error {
	if len(r.Contents) < cacheEntrySizeLimit {
		memcache.Gob.Set(ctx, &memcache.Item{
			Key:    memcacheResponseKey(r.BackendID, r.RequestID),
			Object: r,
		})
	}
	return c.BackingStore.WriteResponse(ctx, r)
}

// ReadResponse reads from the Store the response to the specified request.
//
// If there is no response yet, then the returned value is nil.
func (c *cachingStore) ReadResponse(ctx context.Context, backendID, requestID string) (*Response, error) {
	var r Response
	if _, err := memcache.Gob.Get(ctx, memcacheResponseKey(backendID, requestID), &r); err == nil {
		return &r, nil
	}
	return c.BackingStore.ReadResponse(ctx, backendID, requestID)
}

// DeleteOldRequests deletes from the Store any backend records older than some threshold.
func (c *cachingStore) DeleteOldBackends(ctx context.Context) error {
	return c.BackingStore.DeleteOldBackends(ctx)
}

// DeleteOldRequests deletes from the Store any requests older than some threshold.
func (c *cachingStore) DeleteOldRequests(ctx context.Context) error {
	return c.BackingStore.DeleteOldRequests(ctx)
}

// AddBackend adds the definition of the backend to the store
func (c *cachingStore) AddBackend(ctx context.Context, backend *Backend) error {
	return c.BackingStore.AddBackend(ctx, backend)
}

// ListBackends lists all of the backends.
func (c *cachingStore) ListBackends(ctx context.Context) ([]*Backend, error) {
	return c.BackingStore.ListBackends(ctx)
}

// DeleteBackend deletes the given backend.
func (c *cachingStore) DeleteBackend(ctx context.Context, backendID string) error {
	return c.BackingStore.DeleteBackend(ctx, backendID)
}

// LookupBackend looks up the backend for the given user and path.
func (c *cachingStore) LookupBackend(ctx context.Context, endUser, path string) (string, error) {
	return c.BackingStore.LookupBackend(ctx, endUser, path)
}

// IsBackendUserAllowed checks whether the given user can act as the specified backend.
func (c *cachingStore) IsBackendUserAllowed(ctx context.Context, backendUser, backendID string) (bool, error) {
	return c.BackingStore.IsBackendUserAllowed(ctx, backendUser, backendID)
}

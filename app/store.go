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
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
)

const (
	backendTimeout    = 5 * time.Minute
	sharedBackendUser = "allUsers"

	// Datastore fields cannot exceed 1MB in size
	fieldByteLimit = 1000000
	// A single datastore operation cannot operate on more than 500 entities
	multiOpSizeLimit = 500

	backendKind         = "backend"
	backendTrackerKind  = "backendTracker"
	activityTrackerKind = "activityTracker"
	blobPartsKind       = "blobParts"
	requestKindPrefix   = "req:"
	responseKind        = "response"
)

type blobPart struct {
	ID        string
	Bytes     []byte
	StartTime time.Time
}

type blob struct {
	Inlined []byte
	Parts   []string
}

func writeBlobParts(ctx context.Context, bytes []byte, blobName string, t time.Time) ([]string, error) {
	var partNames []string
	var wg sync.WaitGroup
	partCount := (len(bytes) / fieldByteLimit) + 1
	errs := make(chan error, partCount)
	for i := 0; i < partCount; i++ {
		partStart := i * fieldByteLimit
		partEnd := (i + 1) * fieldByteLimit
		if partEnd > len(bytes) {
			partEnd = len(bytes)
		}
		partBytes := bytes[partStart:partEnd]

		ID := fmt.Sprintf("%s.part%d", blobName, i)
		partNames = append(partNames, ID)

		k := datastore.NewKey(ctx, blobPartsKind, ID, 0, nil)
		p := &blobPart{
			ID:        ID,
			Bytes:     partBytes,
			StartTime: t,
		}
		// You might wonder why we are using a separate Put for each
		// key/part instead of a single PutMulti for all of them...
		//
		// It turns out that the datastore size limit applies not just to
		// the size of individual entries, but also to individual API calls.
		// Since we have more data than is allowed in that limit, we have to
		// break down the write into multiple API calls, and not just a single
		// API call writing multiple entities.
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := datastore.Put(ctx, k, p); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	if len(errs) > 0 {
		return nil, <-errs
	}
	return partNames, nil
}

func newBlob(ctx context.Context, bytes []byte, blobName string, t time.Time) (*blob, error) {
	if len(bytes) < fieldByteLimit {
		return &blob{
			Inlined: bytes,
			Parts:   []string{},
		}, nil
	}

	blobPartNames, err := writeBlobParts(ctx, bytes[fieldByteLimit:], blobName, t)
	if err != nil {
		log.Errorf(ctx, "Failed to write the parts of a blob: %q", err.Error())
		return nil, err
	}
	b := &blob{
		Inlined: bytes[0:fieldByteLimit],
		Parts:   blobPartNames,
	}
	return b, nil
}

func (bp *blob) read(ctx context.Context) ([]byte, error) {
	bytes := bp.Inlined
	if len(bp.Parts) == 0 {
		return bytes, nil
	}

	var keys []*datastore.Key
	var parts []*blobPart
	for _, part := range bp.Parts {
		keys = append(keys, datastore.NewKey(ctx, blobPartsKind, part, 0, nil))
		parts = append(parts, &blobPart{})
	}

	if err := datastore.GetMulti(ctx, keys, parts); err != nil {
		log.Errorf(ctx, "Failed to read the parts of a blob: %q", err.Error())
		return nil, err
	}
	for _, part := range parts {
		bytes = append(bytes, part.Bytes...)
	}
	return bytes, nil
}

type storedRequest struct {
	BackendID string
	RequestID string
	User      string
	StartTime time.Time
	Completed bool

	RequestBytes blob
}

func requestKind(backendID string) string {
	return fmt.Sprintf("%s%q", requestKindPrefix, backendID)
}

func (r *storedRequest) datastoreKey(ctx context.Context) *datastore.Key {
	return datastore.NewKey(ctx, requestKind(r.BackendID), r.RequestID, 0, nil)
}

func newStoredRequest(ctx context.Context, r *Request) (*storedRequest, error) {
	sr := &storedRequest{
		BackendID: r.BackendID,
		RequestID: r.RequestID,
		User:      r.User,
		StartTime: r.StartTime,
		Completed: r.Completed,
	}
	requestBlob, err := newBlob(ctx, r.Contents, fmt.Sprintf("%s.request", r.RequestID), r.StartTime)
	if err != nil {
		return nil, err
	}
	sr.RequestBytes = *requestBlob
	k := sr.datastoreKey(ctx)
	if _, err := datastore.Put(ctx, k, sr); err != nil {
		return nil, err
	}
	return sr, nil
}

func readStoredRequest(ctx context.Context, backendID, requestID string) (*storedRequest, error) {
	key := datastore.NewKey(ctx, requestKind(backendID), requestID, 0, nil)
	var r storedRequest
	if err := datastore.Get(ctx, key, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *storedRequest) toRequest(ctx context.Context) (*Request, error) {
	requestBytes, err := r.RequestBytes.read(ctx)
	if err != nil {
		return nil, err
	}
	return &Request{
		BackendID: r.BackendID,
		RequestID: r.RequestID,
		User:      r.User,
		StartTime: r.StartTime,
		Contents:  requestBytes,
		Completed: r.Completed,
	}, nil
}

type storedResponse struct {
	BackendID     string
	RequestID     string
	ResponseBytes blob
	StartTime     time.Time
	Latency       time.Duration
	ResponseSize  int
}

func (r *storedResponse) datastoreKey(ctx context.Context) *datastore.Key {
	return datastore.NewKey(ctx, responseKind, r.RequestID, 0, nil)
}

func newStoredResponse(ctx context.Context, r *Response) (*storedResponse, error) {
	sr := &storedResponse{
		BackendID:    r.BackendID,
		RequestID:    r.RequestID,
		Latency:      time.Since(r.StartTime),
		ResponseSize: len(r.Contents),
	}
	responseBlob, err := newBlob(ctx, r.Contents, fmt.Sprintf("%s.response", r.RequestID), r.StartTime)
	if err != nil {
		return nil, err
	}
	sr.ResponseBytes = *responseBlob
	k := sr.datastoreKey(ctx)
	if _, err := datastore.Put(ctx, k, sr); err != nil {
		return nil, err
	}
	return sr, nil
}

func readStoredResponse(ctx context.Context, backendID, requestID string) (*storedResponse, error) {
	key := datastore.NewKey(ctx, responseKind, requestID, 0, nil)
	var r storedResponse
	if err := datastore.Get(ctx, key, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *storedResponse) toResponse(ctx context.Context) (*Response, error) {
	responseBytes, err := r.ResponseBytes.read(ctx)
	if err != nil {
		return nil, err
	}
	return &Response{
		BackendID: r.BackendID,
		RequestID: r.RequestID,
		Contents:  responseBytes,
	}, nil
}

// Type persistentStore implements the Store interface and uses the Google Cloud Datastore for storing data.
type persistentStore struct{}

// NewPersistentStore returns a new implementation of the Store interface that uses the
// Google Cloud Datastore as its backing implementation.
func NewPersistentStore() Store {
	return &persistentStore{}
}

// WriteRequest writes the given request to the Datastore.
func (d *persistentStore) WriteRequest(ctx context.Context, r *Request) error {
	_, err := newStoredRequest(ctx, r)
	return err
}

// ReadRequest reads the specified request from the Datastore.
func (d *persistentStore) ReadRequest(ctx context.Context, backendID, requestID string) (*Request, error) {
	sr, err := readStoredRequest(ctx, backendID, requestID)
	if err != nil {
		return nil, err
	}
	return sr.toRequest(ctx)
}

// ListPendingRequests returns the list or requests for the given backend that do not yet have a stored response.
func (d *persistentStore) ListPendingRequests(ctx context.Context, backendID string) ([]string, error) {
	var storedRequests []*storedRequest
	var readErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := d.registerBackendAsSeen(ctx, backendID); err != nil {
			log.Errorf(ctx, "Failed to register a backend [%q]: %q", backendID, err.Error())
		}
	}()
	go func() {
		defer wg.Done()
		q := datastore.NewQuery(requestKind(backendID)).Filter("Completed=", false).Distinct().Limit(100)
		if _, readErr = q.GetAll(ctx, &storedRequests); readErr != nil {
			readErr = fmt.Errorf("Failed to query the pending requests: %q", readErr.Error())
			return
		}
	}()
	wg.Wait()
	if readErr != nil {
		return nil, readErr
	}
	var requests []string
	for _, sr := range storedRequests {
		requests = append(requests, sr.RequestID)
	}
	return requests, nil
}

// WriteResponse writes the given response to the Datastore.
func (d *persistentStore) WriteResponse(ctx context.Context, r *Response) error {
	var err error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		backendID := r.BackendID
		if err := d.registerBackendAsActive(ctx, backendID); err != nil {
			log.Errorf(ctx, "Failed to update a backend's activity tracker [%q]: %q", backendID, err.Error())
		}
	}()
	go func() {
		defer wg.Done()
		_, err = newStoredResponse(ctx, r)
	}()
	wg.Wait()
	return err
}

// ReadResponse reads from the Datastore the response to the specified request.
//
// If there is no response yet, then the returned value is nil.
func (d *persistentStore) ReadResponse(ctx context.Context, backendID, requestID string) (*Response, error) {
	sr, err := readStoredResponse(ctx, backendID, requestID)
	if err != nil {
		return nil, err
	}
	return sr.toResponse(ctx)
}

func deleteRequestsForBackend(ctx context.Context, backend string, cutoff time.Time) error {
	requestsQuery := datastore.NewQuery(requestKind(backend)).Filter("StartTime<", cutoff).Distinct().KeysOnly().Limit(multiOpSizeLimit)
	keys, err := requestsQuery.GetAll(ctx, nil)
	if err != nil {
		return err
	}
	for len(keys) > 0 {
		if err := datastore.DeleteMulti(ctx, keys); err != nil {
			return err
		}
		keys, err = requestsQuery.GetAll(ctx, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// backendTracker is used to track how recently a backend was active.
//
// That, in turn, allows us to garbage collect dead backends.
type backendTracker struct {
	LastSeen time.Time
}

// activityTracker is used to track how recently a user request hit a specific backend.
//
// That can be used by administrators to spin down idle backends.
type activityTracker struct {
	LastActive time.Time
}

func listOldBackends(ctx context.Context) ([]string, error) {
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	backendQuery := datastore.NewQuery(backendTrackerKind).Filter("LastSeen<", oneHourAgo).Distinct().KeysOnly().Limit(multiOpSizeLimit)
	backends, err := backendQuery.GetAll(ctx, nil)
	if err != nil {
		return nil, err
	}

	var results []string
	for _, key := range backends {
		if key != nil {
			results = append(results, key.StringID())
		}
	}
	return results, nil
}

func listRecentBackends(ctx context.Context) ([]string, error) {
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	backendQuery := datastore.NewQuery(backendTrackerKind).Filter("LastSeen>", oneHourAgo).Distinct().KeysOnly()
	backends, err := backendQuery.GetAll(ctx, nil)
	if err != nil {
		return nil, err
	}

	var results []string
	for _, key := range backends {
		if key != nil {
			results = append(results, key.StringID())
		}
	}
	return results, nil
}

// DeleteOldBackends deletes from the datastore any backends older than 1 hour
func (d *persistentStore) DeleteOldBackends(ctx context.Context) error {
	bs, err := listOldBackends(ctx)
	if err != nil {
		return err
	}
	for _, b := range bs {
		if err := d.DeleteBackend(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

// DeleteOldRequests deletes from the datastore any requests older than 2 minutes
func (d *persistentStore) DeleteOldRequests(ctx context.Context) error {
	twoMinutesAgo := time.Now().Add(-2 * time.Minute)

	backends, err := listRecentBackends(ctx)
	if err != nil {
		return err
	}
	for _, backend := range backends {
		if err := deleteRequestsForBackend(ctx, backend, twoMinutesAgo); err != nil {
			return err
		}
	}

	responseQuery := datastore.NewQuery(responseKind).Filter("StartTime<", twoMinutesAgo).Distinct().KeysOnly().Limit(multiOpSizeLimit)
	responseKeys, err := responseQuery.GetAll(ctx, nil)
	if err != nil {
		return err
	}
	if err := datastore.DeleteMulti(ctx, responseKeys); err != nil {
		return err
	}

	contQuery := datastore.NewQuery(blobPartsKind).Filter("StartTime<", twoMinutesAgo).Distinct().KeysOnly().Limit(multiOpSizeLimit)
	contKeys, err := contQuery.GetAll(ctx, nil)
	if err != nil {
		return err
	}
	return datastore.DeleteMulti(ctx, contKeys)
}

// registerBackendAsSeen records in the datastore that we just saw the given backend.
func (d *persistentStore) registerBackendAsSeen(ctx context.Context, backendID string) error {
	key := datastore.NewKey(ctx, backendTrackerKind, backendID, 0, nil)
	t := &backendTracker{
		LastSeen: time.Now(),
	}
	if _, err := datastore.Put(ctx, key, t); err != nil {
		return err
	}
	return nil
}

// registerBackendAsActive records in the datastore that a user just used the given backend.
func (d *persistentStore) registerBackendAsActive(ctx context.Context, backendID string) error {
	key := datastore.NewKey(ctx, activityTrackerKind, backendID, 0, nil)
	t := &activityTracker{
		LastActive: time.Now(),
	}
	if _, err := datastore.Put(ctx, key, t); err != nil {
		return err
	}
	return nil
}

// hasBackend reports whether or not the given backend has been seen recently.
func (d *persistentStore) hasBackend(ctx context.Context, backendID string, lastSeenTimeout time.Duration) bool {
	lastUsed := backendLastSeen(ctx, backendID)
	return lastUsed != nil && time.Since(*lastUsed) < lastSeenTimeout
}

// backendLastSeen looks up how recently we have seen the given backend.
//
// In the event of errors talking to the datastore, we assume the backend
// has never been seen.
func backendLastSeen(ctx context.Context, backendID string) *time.Time {
	key := datastore.NewKey(ctx, backendTrackerKind, backendID, 0, nil)
	var t backendTracker
	if err := datastore.Get(ctx, key, &t); err != nil {
		return nil
	}
	return &t.LastSeen
}

// backendLastActive looks up how recently a user used the given backend.
//
// In the event of errors talking to the datastore, we assume the answer is the Unix epoch.
func backendLastActive(ctx context.Context, backendID string) time.Time {
	key := datastore.NewKey(ctx, activityTrackerKind, backendID, 0, nil)
	var t activityTracker
	if err := datastore.Get(ctx, key, &t); err != nil {
		return time.Unix(0, 0)
	}
	return t.LastActive
}

// AddBackend adds the definition of the backend to the store.
func (d *persistentStore) AddBackend(ctx context.Context, backend *Backend) error {
	// We never store the last-used value in the store. It is an output-only
	// field that is calculated dynamically.
	backend.LastUsed = ""

	backendID := backend.BackendID
	trackerKey := datastore.NewKey(ctx, backendTrackerKind, backendID, 0, nil)
	t := &backendTracker{
		// Set the last active time to old enough that the backend
		// will not get requests (until an agent registers for it),
		// but new enough that the record will not be deleted for a while.
		LastSeen: time.Now().Add(-backendTimeout),
	}
	if _, err := datastore.Put(ctx, trackerKey, t); err != nil {
		return err
	}
	backendKey := datastore.NewKey(ctx, backendKind, backendID, 0, nil)
	if _, err := datastore.Put(ctx, backendKey, backend); err != nil {
		return err
	}
	return nil
}

// ListBackends lists all of the backends.
func (d *persistentStore) ListBackends(ctx context.Context) ([]*Backend, error) {
	var backends []*Backend
	q := datastore.NewQuery(backendKind).Distinct()
	_, err := q.GetAll(ctx, &backends)
	if err != nil {
		return nil, fmt.Errorf("Failed to query the backends: %q", err.Error())
	}
	if backends == nil {
		return []*Backend{}, nil
	}
	for _, b := range backends {
		b.LastUsed = backendLastActive(ctx, b.BackendID).Format(time.RFC3339)
	}
	return backends, nil
}

// DeleteBackend deletes the given backend.
func (d *persistentStore) DeleteBackend(ctx context.Context, backendID string) error {
	txnOpts := &datastore.TransactionOptions{
		XG: true,
	}
	if err := datastore.RunInTransaction(ctx, func(ctx context.Context) error {
		backendKey := datastore.NewKey(ctx, backendKind, backendID, 0, nil)
		trackerKey := datastore.NewKey(ctx, backendTrackerKind, backendID, 0, nil)
		if err := datastore.Delete(ctx, backendKey); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if err := datastore.Delete(ctx, trackerKey); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		return nil
	}, txnOpts); err != nil {
		return err
	}
	return deleteRequestsForBackend(ctx, backendID, time.Now())
}

func mostSpecificMatchingBackend(path string, backends []*Backend) (string, error) {
	var closestMatch string
	var longestMatchingPath string
	for _, b := range backends {
		for _, p := range b.PathPrefixes {
			if strings.HasPrefix(path, p) {
				if closestMatch == "" || len(p) > len(longestMatchingPath) {
					longestMatchingPath = p
					closestMatch = b.BackendID
				}
			}
		}
	}
	if closestMatch == "" {
		return "", fmt.Errorf("Found no matching backends for %q", path)
	}
	return closestMatch, nil
}

// lookupSharedBackend looks up a backend that can serve all users for the given path.
func (d *persistentStore) lookupSharedBackend(ctx context.Context, path string) (string, error) {
	var backends []*Backend
	q := datastore.NewQuery(backendKind).Filter("EndUser=", sharedBackendUser).Distinct()
	if _, err := q.GetAll(ctx, &backends); err != nil {
		return "", fmt.Errorf("Failed to query the backends: %q", err.Error())
	}
	backendID, err := mostSpecificMatchingBackend(path, backends)
	if err != nil {
		return "", err
	}
	if d.hasBackend(ctx, backendID, backendTimeout) {
		return backendID, nil
	}
	return "", fmt.Errorf("No backend for %q", path)
}

// LookupBackend looks up the backend for the given user and path.
func (d *persistentStore) LookupBackend(ctx context.Context, endUser, path string) (string, error) {
	var backends []*Backend
	q := datastore.NewQuery(backendKind).Filter("EndUser=", endUser).Distinct()
	if _, err := q.GetAll(ctx, &backends); err != nil {
		return "", fmt.Errorf("Failed to query the backends: %q", err.Error())
	}
	backendID, err := mostSpecificMatchingBackend(path, backends)
	if err != nil {
		return d.lookupSharedBackend(ctx, path)
	}
	if d.hasBackend(ctx, backendID, backendTimeout) {
		return backendID, nil
	}
	return "", fmt.Errorf("No backend for %q", endUser)
}

// IsBackendUserAllowed checks whether the given user can act as the specified backend.
func (d *persistentStore) IsBackendUserAllowed(ctx context.Context, backendUser, backendID string) (bool, error) {
	key := datastore.NewKey(ctx, backendKind, backendID, 0, nil)
	var b Backend
	if err := datastore.Get(ctx, key, &b); err != nil {
		if err == datastore.ErrNoSuchEntity {
			return false, nil
		}
		return false, err
	}
	return b.BackendUser == backendUser, nil
}

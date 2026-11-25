/*
Copyright 2026 Google Inc. All rights reserved.

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

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPollForNewRequestsCancelsContextEachIteration(t *testing.T) {
	tests := []struct {
		name      string
		roundTrip func(poll int) (*http.Response, error)
	}{
		{
			name: "success",
			roundTrip: func(int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("[]")),
					Header:     make(http.Header),
				}, nil
			},
		},
		{
			name: "error_then_success",
			roundTrip: func(poll int) (*http.Response, error) {
				if poll == 1 {
					return nil, errors.New("transient poll error")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("[]")),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origProxy := *proxy
			origTimeout := *proxyTimeout
			*proxy = "http://proxy.example/"
			*proxyTimeout = time.Minute
			t.Cleanup(func() {
				*proxy = origProxy
				*proxyTimeout = origTimeout
			})

			pollingCtx, stopPolling := context.WithCancel(context.Background())
			defer stopPolling()

			const totalPolls = 5
			var (
				polls   int
				prevCtx context.Context
			)

			client := &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					polls++
					if prevCtx != nil {
						if err := prevCtx.Err(); !errors.Is(err, context.Canceled) {
							t.Errorf("poll %d: previous poll context was not canceled: got %v, want %v", polls, err, context.Canceled)
						}
					}
					prevCtx = req.Context()
					if polls >= totalPolls {
						stopPolling()
					}
					return tc.roundTrip(polls)
				}),
			}

			pollForNewRequests(pollingCtx, client, http.NotFoundHandler(), "test-backend")

			if polls != totalPolls {
				t.Fatalf("got %d polls, want %d", polls, totalPolls)
			}
			if prevCtx == nil {
				t.Fatal("expected at least one poll context")
			}
			if err := prevCtx.Err(); !errors.Is(err, context.Canceled) {
				t.Errorf("final poll context was not canceled: got %v, want %v", err, context.Canceled)
			}
		})
	}
}
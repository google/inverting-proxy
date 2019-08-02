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

package banner

import (
	"net/http"
	"net/url"
	"testing"
)

func TestIsHTMLRequest(t *testing.T) {
	testCases := []struct {
		req  *http.Request
		want bool
	}{
		{
			req:  &http.Request{},
			want: false,
		},
		{
			req: &http.Request{
				Method: http.MethodPost,
				Header: http.Header{
					"Accept": []string{"text/html"},
				},
			},
			want: false,
		},
		{
			req: &http.Request{
				Method: http.MethodGet,
				Header: http.Header{
					"Accept": []string{"application/json"},
				},
			},
			want: false,
		},
		{
			req: &http.Request{
				Method: http.MethodGet,
				Header: http.Header{
					"Accept": []string{"text/xhtml"},
				},
			},
			want: true,
		},
		{
			req: &http.Request{
				Method: http.MethodGet,
				Header: http.Header{
					"Accept": []string{"text/html"},
				},
			},
			want: true,
		},
	}

	for _, testCase := range testCases {
		if got, want := isHTMLRequest(testCase.req), testCase.want; got != want {
			t.Errorf("isHTMLRequest(%+v): got %v, want %v", testCase.req, got, want)
		}
	}
}

func TestIsHTMLResponse(t *testing.T) {
	testCases := []struct {
		resp *http.Response
		want bool
	}{
		{
			resp: &http.Response{},
			want: false,
		},
		{
			resp: &http.Response{
				StatusCode: http.StatusNotFound,
				Header: http.Header{
					"Content-Type": []string{"text/html"},
				},
			},
			want: false,
		},
		{
			resp: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
			},
			want: false,
		},
		{
			resp: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/xhtml"},
				},
			},
			want: true,
		},
		{
			resp: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/html"},
				},
			},
			want: true,
		},
	}

	for _, testCase := range testCases {
		if got, want := isHTMLResponse(testCase.resp), testCase.want; got != want {
			t.Errorf("isHTMLResponse(%+v): got %v, want %v", testCase.resp, got, want)
		}
	}
}

func TestIsAlreadyFramed(t *testing.T) {
	testCases := []struct {
		req  *http.Request
		want bool
	}{
		{
			req:  &http.Request{},
			want: false,
		},
		{
			req: &http.Request{
				Host: "example.com",
				URL: &url.URL{
					Path: "/some/example/path",
				},
				Header: http.Header{
					"Referer": []string{"https://example.com/some/other/path"},
				},
			},
			want: false,
		},
		{
			req: &http.Request{
				Host: "example.com",
				URL: &url.URL{
					Path: "/some/example/path",
				},
				Header: http.Header{
					"Referer": []string{"https://example.com/some/example/path"},
				},
			},
			want: true,
		},
	}

	for _, testCase := range testCases {
		if got, want := isAlreadyFramed(testCase.req), testCase.want; got != want {
			t.Errorf("isAlreadyFramed(%+v): got %v, want %v", testCase.req, got, want)
		}
	}
}

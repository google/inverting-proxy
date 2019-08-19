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

package sessions

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
)

const (
	backendCookie   = "backend-cookie"
	sessionCookie   = "proxy-sessions-cookie"
	sessionLifetime = 10 * time.Second
	sessionCount    = 100
)

func backendHandler(t *testing.T) http.Handler {
	backendCookieVal := uuid.New().String()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bc, err := r.Cookie(backendCookie)
		if err == http.ErrNoCookie || bc == nil {
			bc = &http.Cookie{
				Name:     backendCookie,
				Value:    backendCookieVal,
				HttpOnly: true,
			}
			http.SetCookie(w, bc)
			http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
			return
		}
		if got, want := bc.Value, backendCookieVal; got != want {
			t.Errorf("Unexepected backend cookie value: got %q, want %q", got, want)
		}
		w.Write([]byte("OK"))
	})
}

func TestSessionsEnabled(t *testing.T) {
	jarOptions := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	jar, err := cookiejar.New(&jarOptions)
	if err != nil {
		t.Fatalf("Failure creating a cookie jar: %v", err)
	}

	testHandler := backendHandler(t)
	c := NewCache(sessionCookie, sessionLifetime, sessionCount, true)
	h := c.SessionHandler(testHandler)
	testServer := httptest.NewServer(h)
	defer testServer.Close()

	serverURL, err := url.Parse(testServer.URL)
	if err != nil {
		t.Fatalf("Internal error setting up the test server... failed to parse the server URL: %v", err)
	}

	client := *testServer.Client()
	client.Jar = jar
	if resp, err := (&client).Get(testServer.URL); err != nil {
		t.Errorf("Failure getting a response for a request proxied with sessions: %v", err)
	} else if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("Unexpected response from a request proxied with sessions: got %d, want %d", got, want)
	}

	cookies := jar.Cookies(serverURL)
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Errorf("Unexpected cookies found when proxying a request with sessions: %v", cookies)
	}
}

func TestSessionsDisabled(t *testing.T) {
	jarOptions := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	jar, err := cookiejar.New(&jarOptions)
	if err != nil {
		t.Fatalf("Failure creating a cookie jar: %v", err)
	}

	testHandler := backendHandler(t)
	var c *Cache
	h := c.SessionHandler(testHandler)
	testServer := httptest.NewServer(h)
	defer testServer.Close()

	serverURL, err := url.Parse(testServer.URL)
	if err != nil {
		t.Fatalf("Internal error setting up the test server... failed to parse the server URL: %v", err)
	}

	client := *testServer.Client()
	client.Jar = jar
	if resp, err := (&client).Get(testServer.URL); err != nil {
		t.Errorf("Failure getting a response for a request proxied without sessions: %v", err)
	} else if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("Unexpected response from a request proxied without sessions: got %d, want %d", got, want)
	}

	cookies := jar.Cookies(serverURL)
	if len(cookies) != 1 || cookies[0].Name != backendCookie {
		t.Errorf("Unexpected cookies found when proxying a request without sessions: %v", cookies)
	}
}

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

// Package sessions implements proxy-side user session tracking for reverse proxies.
package sessions

import (
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"time"

	"github.com/golang/groupcache/lru"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
)

type sessionResponseWriter struct {
	c             *Cache
	sessionID     string
	urlForCookies *url.URL

	wrapped     http.ResponseWriter
	wroteHeader bool
}

func (w *sessionResponseWriter) Header() http.Header {
	return w.wrapped.Header()
}

func (w *sessionResponseWriter) Write(bs []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.wrapped.Write(bs)
}

func (w *sessionResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		// Multiple calls ot WriteHeader are no-ops
		return
	}
	w.wroteHeader = true
	header := w.Header()
	cookiesToAdd := (&http.Response{Header: header}).Cookies()
	if len(cookiesToAdd) == 0 {
		// There were no cookies to intercept
		w.wrapped.WriteHeader(statusCode)
		return
	}
	header.Del("Set-Cookie")
	if w.sessionID == "" {
		// No session was previously defined, so we need to create a new one
		w.sessionID = uuid.New().String()
		sessionCookie := &http.Cookie{
			Name:     w.c.sessionCookieName,
			Value:    w.sessionID,
			Path:     "/",
			Secure:   !w.c.disableSSLForTest,
			HttpOnly: true,
			Expires:  time.Now().Add(w.c.sessionCookieTimeout),
		}
		header.Add("Set-Cookie", sessionCookie.String())
	}
	cookieJar, err := w.c.cachedCookieJar(w.sessionID)
	if err != nil {
		log.Printf("Failure reading a cached cookie jar: %v", err)
	}
	cookieJar.SetCookies(w.urlForCookies, cookiesToAdd)
	w.wrapped.WriteHeader(statusCode)
}

type sessionHandler struct {
	c       *Cache
	wrapped http.Handler
}

func (h *sessionHandler) extractSessionID(r *http.Request) string {
	sessionCookie, err := r.Cookie(h.c.sessionCookieName)
	if err != nil || sessionCookie == nil {
		// There is no session cookie, so we do not (yet) have a session
		return ""
	}
	return sessionCookie.Value
}

func (h *sessionHandler) restoreSession(r *http.Request, cachedCookies []*http.Cookie) {
	// Remove the session cookie
	existingCookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range existingCookies {
		if c.Name != h.c.sessionCookieName {
			r.AddCookie(c)
		}
	}

	// Restore any cached cookies from the session
	for _, c := range cachedCookies {
		r.AddCookie(c)
	}
}

// ServeHTTP implements the http.Handler interface
func (h *sessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlForCookies := *(r.URL)
	urlForCookies.Scheme = "https"
	urlForCookies.Host = r.Host
	sessionID := h.extractSessionID(r)
	cachedCookieJar, err := h.c.cachedCookieJar(sessionID)
	if err != nil {
		// There is a session cookie but we could not fetch the corresponding cookie jar.
		//
		// This should not happen and represents an internal error in the session handling logic.
		log.Printf("Failure reading the cookie jar for session %q: %v", sessionID, err)
		http.Error(w, fmt.Sprintf("Internal error reading the session %q", sessionID), http.StatusInternalServerError)
		return
	}
	cachedCookies := cachedCookieJar.Cookies(&urlForCookies)
	h.restoreSession(r, cachedCookies)
	w = &sessionResponseWriter{
		c:             h.c,
		sessionID:     sessionID,
		urlForCookies: &urlForCookies,
		wrapped:       w,
	}
	h.wrapped.ServeHTTP(w, r)
}

// SessionHandler returns an instance of `http.Handler` that wraps the given handler and adds proxy-side session tracking.
func (cj *Cache) SessionHandler(wrapped http.Handler) http.Handler {
	if cj == nil {
		return wrapped
	}
	return &sessionHandler{
		c:       cj,
		wrapped: wrapped,
	}
}

// Cache represents a LRU cache to store sessions
type Cache struct {
	sessionCookieName    string
	sessionCookieTimeout time.Duration
	disableSSLForTest    bool

	cache *lru.Cache
	mu    sync.Mutex
}

// NewCache initializes an LRU session cache
func NewCache(sessionCookieName string, sessionCookieTimeout time.Duration, cookieCacheLimit int, disableSSLForTest bool) (*Cache, error) {
	return &Cache{
		sessionCookieName:    sessionCookieName,
		sessionCookieTimeout: sessionCookieTimeout,
		disableSSLForTest:    disableSSLForTest,
		cache:                lru.New(cookieCacheLimit),
	}, nil
}

// addJarToCache takes a Jar from http.Client and stores it in a cache
func (cj *Cache) addJarToCache(sessionID string, jar http.CookieJar) {
	cj.mu.Lock()
	cj.cache.Add(sessionID, jar)
	cj.mu.Unlock()
}

// cachedCookieJar returns the CookieJar mapped to the sessionID
func (cj *Cache) cachedCookieJar(sessionID string) (jar http.CookieJar, err error) {
	val, ok := cj.cache.Get(sessionID)
	if !ok {
		options := cookiejar.Options{
			PublicSuffixList: publicsuffix.List,
		}
		jar, err = cookiejar.New(&options)
		cj.addJarToCache(sessionID, jar)
		return jar, err
	}

	jar, ok = val.(http.CookieJar)
	if !ok {
		return nil, fmt.Errorf("Internal error; unexpected type for value (%+v) stored in the cookie jar cache", val)
	}
	return jar, nil
}

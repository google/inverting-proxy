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
	cj            *Cache
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
	w.cj.interceptSession(w.sessionID, w, w.urlForCookies)
	w.wrapped.WriteHeader(statusCode)
}

type sessionHandler struct {
	cj      *Cache
	wrapped http.Handler
}

func (h *sessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlForCookies := *(r.URL)
	urlForCookies.Scheme = "https"
	urlForCookies.Host = r.Host
	sessionID := h.cj.extractAndRestoreSession(r, &urlForCookies)
	w = &sessionResponseWriter{
		cj:            h.cj,
		sessionID:     sessionID,
		urlForCookies: &urlForCookies,
		wrapped:       w,
	}
	h.wrapped.ServeHTTP(w, r)
}

// SessionHandler returns an instance of `http.Handler` that wraps the given handler and adds proxy-side session tracking.
func (cj *Cache) SessionHandler(wrapped http.Handler) http.Handler {
	return &sessionHandler{
		cj:      cj,
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

// interceptSession modifies the given ResponseWriter by removing any Set-Cookie headers
// and instead adding those cookies to the corresponding session.
//
// If there is not already a session, and we have new cookies to save in the session, then
// this method will create a new session, and set a session cookie for it.
//
// This is the inverse of extractAndRestoreSession.
func (cj *Cache) interceptSession(sessionID string, w http.ResponseWriter, u *url.URL) error {
	if cj == nil {
		return nil
	}

	header := w.Header()
	cookiesToAdd := (&http.Response{Header: header}).Cookies()
	if len(cookiesToAdd) == 0 {
		// There were no cookies to intercept
		return nil
	}

	header.Del("Set-Cookie")
	if sessionID == "" {
		// No session was previously defined, so we need to create a new one
		sessionID = uuid.New().String()
		sessionCookie := &http.Cookie{
			Name:     cj.sessionCookieName,
			Value:    sessionID,
			Path:     "/",
			Secure:   !cj.disableSSLForTest,
			HttpOnly: true,
			Expires:  time.Now().Add(cj.sessionCookieTimeout),
		}
		header.Add("Set-Cookie", sessionCookie.String())
	}

	cookieJar, err := cj.cachedCookieJar(sessionID)
	if err != nil {
		log.Printf("Failure reading a cached cookie jar: %v", err)
		return fmt.Errorf("Failure reading a cached cookie jar: %v", err)
	}
	cookieJar.SetCookies(u, cookiesToAdd)
	return nil
}

// extractAndRestoreSession pulls the session ID cookie (if any) out of the given request,
// finds the corresponding session, and then adds any saved cookies for that session to the request.
//
// The return value is the session ID, or an empty string if there is no session.
//
// This is the inverse of interceptSession.
func (cj *Cache) extractAndRestoreSession(r *http.Request, u *url.URL) (sessionID string) {
	if cj == nil {
		return ""
	}

	sessionCookie, err := r.Cookie(cj.sessionCookieName)
	if err != nil || sessionCookie == nil {
		// There is no session cookie, so we have nothing to do.
		return ""
	}

	sessionID = sessionCookie.Value
	cachedCookieJar, err := cj.cachedCookieJar(sessionID)
	if err != nil {
		log.Printf("Failure reading the cookie jar for session %q: %v", sessionID, err)
		// We are unable to fetch a cookie jar for the session, so we have no
		// existing, cached cookies to insert into the request.
		return ""
	}

	// Remove the session cookie
	existingCookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range existingCookies {
		if c.Name != cj.sessionCookieName {
			r.AddCookie(c)
		}
	}

	// Restore any cached cookies from the session
	cachedCookies := cachedCookieJar.Cookies(u)
	for _, c := range cachedCookies {
		r.AddCookie(c)
	}
	return sessionID
}

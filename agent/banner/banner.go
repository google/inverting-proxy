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
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"
)

const (
	acceptHeader        = "Accept"
	cacheControlHeader  = "Cache-Control"
	contentTypeHeader   = "Content-Type"
	dateHeader          = "Date"
	expiresHeader       = "Expires"
	refererHeader       = "Referer"
	pragmaHeader        = "Pragma"
	secFetchModeHeader  = "Sec-Fetch-Mode"
	xFrameOptionsHeader = "X-Frame-Options"

	frameWrapperTemplate = `<html>
  <head>
    <meta http-equiv="cache-control" content="no-cache" />
  </head>
  <body style="margin:0px">
    <div height="{{.BannerHeight}}">
      {{.Banner}}
    </div>
    <iframe width="100%" id="inverting-proxy-frame" onLoad="window.history.replaceState(null, '', this.contentWindow.location)" style="border:0px;height: calc(100% - {{.BannerHeight}})" src="{{.TargetURL}}"></iframe>
  </body>
</html>`
)

var frameWrapperTmpl = template.Must(template.New("frame-wrapper").Parse(frameWrapperTemplate))

func isHTMLRequest(r *http.Request) bool {
	// We want to err on the side of not injecting a banner in case that might
	// interfere with the semantics of the app. Since we can't know the expected
	// semantics for the response to a POST reqest, we play it safe and only
	// inject the banner in responses to GET requests.
	if r.Method != http.MethodGet {
		return false
	}
	accept := r.Header.Get(acceptHeader)
	// Our injected response will be HTML, so we don't want to inject it unless
	// the client explicitly stated it will accept HTML responses.
	return strings.Contains(accept, "text/html")
}

func isHTMLResponse(statusCode int, responseHeader http.Header) bool {
	if statusCode != http.StatusOK {
		return false
	}
	contentType := responseHeader.Get(contentTypeHeader)
	return strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml")
}

func isAlreadyFramed(r *http.Request) bool {
	if r.Header.Get(secFetchModeHeader) == "nested-navigate" {
		// If the browser told us the page is already framed, then believe it.
		return true
	}
	if referer := r.Header.Get(refererHeader); referer != "" {
		refererURL, err := url.Parse(referer)
		if err == nil && refererURL.Host == r.Host && refererURL.Path == r.URL.Path {
			return true
		}
	}
	return false
}

type bannerResponseWriter struct {
	wrapped      http.ResponseWriter
	bannerHTML   string
	bannerHeight string
	targetURL    *url.URL

	wroteHeader     bool
	writeBytes      bool
	isAlreadyFramed bool
}

func (w *bannerResponseWriter) Header() http.Header {
	return w.wrapped.Header()
}

func setNotCacheable(h http.Header) {
	h.Set(cacheControlHeader, "no-cache, no-store, max-age=0, must-revalidate")
	h.Set(dateHeader, time.Now().UTC().Format(http.TimeFormat))
	h.Set(expiresHeader, time.Time{}.UTC().Format(http.TimeFormat))
	h.Set(pragmaHeader, "no-cache")
}

func setXFrameOptionsSameOrigin(h http.Header) {
	h.Del(xFrameOptionsHeader)
	h.Set(xFrameOptionsHeader, "sameorigin")
}

func (w *bannerResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if !isHTMLResponse(statusCode, w.Header()) {
		w.wrapped.WriteHeader(statusCode)
		w.writeBytes = true
		return
	}
	setNotCacheable(w.Header())
	setXFrameOptionsSameOrigin(w.Header())
	if w.isAlreadyFramed {
		w.wrapped.WriteHeader(statusCode)
		w.writeBytes = true
		return
	}

	var templateBuf bytes.Buffer
	templateVals := &struct {
		TargetURL    string
		Banner       string
		BannerHeight string
	}{
		TargetURL:    w.targetURL.String(),
		Banner:       w.bannerHTML,
		BannerHeight: w.bannerHeight,
	}
	if err := frameWrapperTmpl.Execute(&templateBuf, templateVals); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.wrapped.WriteHeader(statusCode)
	w.wrapped.Write(templateBuf.Bytes())
}

func (w *bannerResponseWriter) Write(bs []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.writeBytes {
		return len(bs), nil
	}
	return w.wrapped.Write(bs)
}

// Proxy builds an HTTP handler that proxies to a wrapped handler but injects the given HTML banner into every HTML response.
func Proxy(ctx context.Context, wrapped http.Handler, bannerHTML, bannerHeight string) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !isHTMLRequest(r) {
			wrapped.ServeHTTP(w, r)
			return
		}
		w = &bannerResponseWriter{
			wrapped:         w,
			bannerHTML:      bannerHTML,
			bannerHeight:    bannerHeight,
			targetURL:       r.URL,
			isAlreadyFramed: isAlreadyFramed(r),
		}
		wrapped.ServeHTTP(w, r)
	})
	return mux, nil
}

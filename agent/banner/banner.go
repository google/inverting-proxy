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
)

const (
	acceptHeader         = "Accept"
	contentTypeHeader    = "Content-Type"
	refererHeader        = "Referer"
	frameWrapperTemplate = `<html>
  <body style="margin:0px">
    {{.Banner}}
    <iframe height="100%" width="100%" style="border:0px" src="{{.TargetURL}}"></iframe>
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
	if referer := r.Header.Get(refererHeader); referer != "" {
		refererURL, err := url.Parse(referer)
		if err == nil && refererURL.Host == r.Host && refererURL.Path == r.URL.Path {
			return true
		}
	}
	return false
}

type bannerResponseWriter struct {
	wrapped    http.ResponseWriter
	bannerHTML string
	targetURL  *url.URL

	wroteHeader bool
	writeBytes  bool
}

func (w *bannerResponseWriter) Header() http.Header {
	return w.wrapped.Header()
}

func (w *bannerResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.wrapped.WriteHeader(statusCode)
	if !isHTMLResponse(statusCode, w.Header()) {
		w.writeBytes = true
		return
	}

	var templateBuf bytes.Buffer
	templateVals := &struct {
		TargetURL string
		Banner    string
	}{
		TargetURL: w.targetURL.String(),
		Banner:    w.bannerHTML,
	}
	if err := frameWrapperTmpl.Execute(&templateBuf, templateVals); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
func Proxy(ctx context.Context, wrapped http.Handler, bannerHTML string) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !isHTMLRequest(r) {
			wrapped.ServeHTTP(w, r)
			return
		}
		if isAlreadyFramed(r) {
			// The page is already framed
			wrapped.ServeHTTP(w, r)
			return
		}
		w = &bannerResponseWriter{
			wrapped:    w,
			bannerHTML: bannerHTML,
			targetURL:  r.URL,
		}
		wrapped.ServeHTTP(w, r)
	})
	return mux, nil
}

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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"text/template"
)

const (
	acceptHeader          = "Accept"
	contentEncodingHeader = "Content-Encoding"
	contentTypeHeader     = "Content-Type"
	refererHeader         = "Referer"
	frameWrapperTemplate  = `<html>
  <body style="margin:0px">
    {{.Banner}}
    <iframe height="100%" width="100%" style="border:0px" src="{{.TargetURL}}"></iframe>
  </body>
</html>`
)

var frameWrapperTmpl = template.Must(template.New("frame-wrapper").Parse(frameWrapperTemplate))

func isHTMLRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	accept := r.Header.Get(acceptHeader)
	return strings.Contains(accept, "text/html")
}

func isHTMLResponse(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if !strings.Contains(resp.Header.Get(contentTypeHeader), "html") {
		return false
	}
	return true
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

func copyRecordedResponse(w http.ResponseWriter, resp *http.Response) {
	for name, vals := range resp.Header {
		for _, val := range vals {
			w.Header().Add(name, val)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	resp.Body.Close()
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
		responseRecorder := httptest.NewRecorder()
		wrapped.ServeHTTP(responseRecorder, r)
		result := responseRecorder.Result()
		if !isHTMLResponse(result) {
			copyRecordedResponse(w, result)
			return
		}

		var templateBuf bytes.Buffer
		templateVals := &struct {
			TargetURL string
			Banner    string
		}{
			TargetURL: r.URL.String(),
			Banner:    bannerHTML,
		}
		if err := frameWrapperTmpl.Execute(&templateBuf, templateVals); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(templateBuf.Bytes())
	})
	return mux, nil
}

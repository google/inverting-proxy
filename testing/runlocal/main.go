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

// Command runlocal launches a reverse proxy that can be used to
// locally test changes to the code in the agent or server packages
//
// Example usage:
//
//	go build -o ~/bin/inverting-proxy-run-local testing/runlocal/main.go
//	~/bin/inverting-proxy-run-local --port 8081
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"html/template"
)

const responseTemplate = `<html>
  <head><title>Proxied response from {{.Path}}</title></head>
  <body>Received a request to {{.Path}} with backend cookie {{.BackendCookie}}</body>
</html>
`

var (
	port     = flag.Int("port", 0, "Port on which to listen")
	respTmpl = template.Must(template.New("response").Parse(responseTemplate))
)

// RunLocalProxy runs a proxy locally
func RunLocalProxy(ctx context.Context) (int, error) {
	// This assumes that "Make build" has been run
	proxyArgs := fmt.Sprintf("${GOPATH}/bin/inverting-proxy --port=%d", *port)
	proxyCmd := exec.CommandContext(ctx, "/bin/bash", "-c", proxyArgs)

	var proxyOut bytes.Buffer
	proxyCmd.Stdout = &proxyOut
	proxyCmd.Stderr = &proxyOut
	if err := proxyCmd.Start(); err != nil {
		log.Fatalf("Failed to start the inverting-proxy binary: %v", err)
	}
	go func() {
		err := proxyCmd.Wait()
		log.Printf("Proxy result: %v, stdout/stderr: %q", err, proxyOut.String())
	}()
	for i := 0; i < 30; i++ {
		for _, line := range strings.Split(proxyOut.String(), "\n") {
			if strings.Contains(line, "Listening on [::]:") {
				portStr := strings.TrimSpace(strings.Split(line, "Listening on [::]:")[1])
				return strconv.Atoi(portStr)
			}
		}
		log.Printf("Waiting for the locally running proxy to start...")
		time.Sleep(1 * time.Second)
	}
	return 0, fmt.Errorf("Locally-running proxy failed to start up in time: %q", proxyOut.String())
}

func main() {
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendHomeDir, err := ioutil.TempDir("", "backend-home")
	if err != nil {
		log.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	gcloudCfg := filepath.Join(backendHomeDir, ".config", "gcloud")
	if err := os.MkdirAll(gcloudCfg, os.ModePerm); err != nil {
		log.Fatalf("Failed to set up a temporary home directory for the test: %v", err)
	}
	fakeMetadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Emulate slow responses from the metadata server, to check that the agent
		// is appropriately caching the results.
		time.Sleep(50 * time.Millisecond)
		if strings.HasPrefix(r.URL.Path, "/computeMetadata/v1/project/project-id") {
			io.WriteString(w, "12345")
			return
		}
		if !(strings.HasPrefix(r.URL.Path, "/computeMetadata/v1/instance/service-accounts/") && strings.HasSuffix(r.URL.Path, "/token")) {
			io.WriteString(w, "ok")
			return
		}
		var fakeToken struct {
			AccessToken  string `json:"access_token"`
			ExpiresInSec int    `json:"expires_in"`
			TokenType    string `json:"token_type"`
		}
		fakeToken.AccessToken = "fakeToken"
		fakeToken.ExpiresInSec = 1000
		fakeToken.TokenType = "Bearer"
		if err := json.NewEncoder(w).Encode(&fakeToken); err != nil {
			log.Printf("Failed to encode a fake service account credential: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))

	backendCookieName := "BackendCookie"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Responding to backend request to %q", r.URL.Path)
		bc, err := r.Cookie(backendCookieName)
		if err == http.ErrNoCookie || bc == nil {
			backendCookieVal := uuid.New().String()
			bc = &http.Cookie{
				Name:     backendCookieName,
				Value:    backendCookieVal,
				HttpOnly: true,
			}
			http.SetCookie(w, bc)
			http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
			return
		}

		var templateBuf bytes.Buffer
		templateVals := &struct {
			Path          string
			BackendCookie string
		}{
			Path:          r.URL.Path,
			BackendCookie: bc.Value,
		}
		if err := respTmpl.Execute(&templateBuf, templateVals); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Add("Content-Type", "text/html")
		w.Write(templateBuf.Bytes())
	}))

	go func() {
		<-ctx.Done()
		backend.Close()
		fakeMetadata.Close()
	}()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		log.Fatalf("Failed to parse the backend URL: %v", err)
	}
	proxyPort, err := RunLocalProxy(ctx)
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	if err != nil {
		log.Fatalf("Failed to run the local inverting proxy: %v", err)
	}
	log.Printf("Started backend at localhost:%s and proxy at %s", backendURL.Port(), proxyURL)

	// This assumes that "Make build" has been run
	args := strings.Join(append(
		[]string{"${GOPATH}/bin/proxy-forwarding-agent"},
		"--debug=true",
		"--disable-ssl-for-test=true",
		"--session-cookie-name=SessionID",
		"--backend=testBackend",
		"--proxy", proxyURL+"/",
		"--host=localhost:"+backendURL.Port(),
		"--inject-banner=\\<div\\ style=\"width:100%;color:white;background-color:#1a73e8;height:24;font-size:20;padding:8px\"\\>Inverting\\ Proxy\\</div\\>"),
		" ")
	agentCmd := exec.CommandContext(ctx, "/bin/bash", "-c", args)
	agentCmd.Stdout = os.Stdout
	agentCmd.Stderr = os.Stderr
	agentCmd.Env = append(os.Environ(), "PATH=", "HOME="+backendHomeDir, "GCE_METADATA_HOST="+strings.TrimPrefix(fakeMetadata.URL, "http://"))
	if err := agentCmd.Start(); err != nil {
		log.Fatalf("Failed to start the agent binary: %v", err)
	}
	defer func() {
		cancel()
		err := agentCmd.Wait()
		log.Printf("Agent result: %v", err)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
}

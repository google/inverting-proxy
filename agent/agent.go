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

// Command agent forwards requests from an inverting proxy to a backend server.
//
// To build, run:
//
//    $ make
//
// And to use, run:
//
//    $ $(GOPATH)/bin/proxy-forwarding-agent -proxy <proxy-url> -backend <backend-ID>

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/golang/groupcache/lru"
	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/google/inverting-proxy/agent/utils"
)

const (
	requestCacheLimit = 1000
	emailScope        = "email"

	maxBackoffDuration = 100 * time.Millisecond

	contentTypeHeader     = "Content-Type"
	websocketShimTemplate = `
<!--START_WEBSOCKET_SHIM-->
<!--
    This file was served from behind a reverse proxy that does not support websockets.

    The following code snippet has been inserted by the proxy to replace websockets
    with socket.io which is based on HTTP and will work with this proxy.

    If this snippet insertion is causing issues, then contact the server administrator.
-->
<script>
(function() {
    if (typeof window.nativeWebSocket !== 'undefined') {
      // We have already replaced websockets
      return;
    }
    console.log('Replacing native websockets with a shim');
    window.nativeWebSocket = window.WebSocket;
    const  location = window.location;
    const shimUri = location.protocol + '//' + location.host + '/{{.ShimPath}}/';

    function shouldShimWebsockets(url) {
      var parsedURL = new URL(url);
      if (typeof parsedURL.host == 'undefined') {
        parsedURL.host = location.host;
      }
      return (parsedURL.host == location.host);
    }

    function WebSocketShim(url, protocols) {
      if (!shouldShimWebsockets(url)) {
        console.log("Not shimming websockets for " + parsedURL.host + " as it does not match the location of " + location.host);
        return new window.nativeWebSocket(url, protocols);
      }

      // We need to reference "this" within nested functions, so we alias it to "self"
      var self = this;

      this.readyState = WebSocketShim.CONNECTING;
      function openedHandler(msg) {
        self.readyState = WebSocketShim.OPEN;
        if (self.onopen) {
          self.onopen({ target: self });
        }
      }
      function receiveHandler(msg) {
        if (self.onmessage) {
          self.onmessage({ target: self, data: msg });
        }
      }
      function errorHandler() {
        if (self.onerror) {
          self.onerror({ target: self });
        }
      }
      self.xhr = function(action, msg, onsuccess, onexit) {
        var req = new XMLHttpRequest();
        req.onreadystatechange = function() {
          if (req.readyState === 4) {
            if (req.status === 200) {
              if (onsuccess) {
                onsuccess(req.responseText);
              }
            } else if (req.status !== 408) {
              errorHandler();
            }
            if (onexit) {
              onexit();
            }
          }
        };
        req.open("POST", shimUri + action, true);
        if (typeof msg !== 'string') {
          msg = JSON.stringify(msg);
        }
        req.send(msg);
      }

      self.closedHandler = function() {
        self.readyState = WebSocketShim.CLOSED;
        if (self.onclose) {
          self.onclose({ target: self });
        }
      }

      function poll() {
        if (self.readyState != WebSocketShim.OPEN) {
          return;
        }
        self.xhr('poll', {'id': self._sessionID}, receiveHandler, poll);
      }

      self.xhr('open', url, function(resp) {
        respJSON = JSON.parse(resp);
        self._sessionID = respJSON.id;
        openedHandler(respJSON.msg);
        poll();
      });
    }
    WebSocketShim.prototype = {
      binaryType: "blob",
      onopen: null,
      onclose: null,
      onmessage: null,
      onerror: null,

      send: function(data) {
        if (this.readyState != WebSocketShim.OPEN) {
          throw new Error('WebSocket is not yet opened');
        }
        this.xhr('data', {'id': this._sessionID, 'msg': data});
      },
      close: function() {
        if (this.readyState != WebSocketShim.OPEN) {
          return;
        }
        this.readyState = WebSocketShim.CLOSING;
        this.xhr('close', {'id': this._sessionID}, false, this.closedHandler);
      },
    };
    WebSocketShim.CONNECTING = 0;
    WebSocketShim.OPEN = 1;
    WebSocketShim.CLOSING = 2;
    WebSocketShim.CLOSED = 3;

    window.WebSocket = WebSocketShim;
})();
</script>
<!--END_WEBSOCKET_SHIM-->
`
)

var (
	proxy                = flag.String("proxy", "", "URL (including scheme) of the inverting proxy")
	host                 = flag.String("host", "localhost:8080", "Hostname (including port) of the backend server")
	backendID            = flag.String("backend", "", "Unique ID for this backend.")
	debug                = flag.Bool("debug", false, "Whether or not to print debug log messages")
	forwardUserID        = flag.Bool("forward-user-id", false, "Whether or not to include the ID (email address) of the end user in requests to the backend")
	shimWebsockets       = flag.Bool("shim-websockets", false, "Whether or not to replace websockets with socket.io")
	shimPath             = flag.String("shim-path", "websocket-shim", "Path under which to handle websocket shim requests")
	healthCheckPath      = flag.String("health-check-path", "/", "Path on backend host to issue health checks against.  Defaults to the root.")
	healthCheckFreq      = flag.Int("health-check-interval-seconds", 0, "Wait time in seconds between health checks.  Set to zero to disable health checks.  Checks disabled by default.")
	healthCheckUnhealthy = flag.Int("health-check-unhealthy-threshold", 2, "A so-far healthy backend will be marked unhealthy after this many consecutive failures. The minimum value is 1.")

	websocketShimTmpl = template.Must(template.New("client-shim").Parse(websocketShimTemplate))
)

type shimmedBody struct {
	reader io.Reader
	closer io.Closer
}

func (sb *shimmedBody) Read(p []byte) (n int, err error) {
	return sb.reader.Read(p)
}

func (sb *shimmedBody) Close() error {
	return sb.closer.Close()
}

func shimBody(resp *http.Response, webSocketShim string) error {
	if resp == nil || resp.Body == nil {
		// We have nothing to do on an empty response
		return nil
	}
	contentType := strings.ToLower(resp.Header.Get(contentTypeHeader))
	if !strings.Contains(contentType, "html") {
		// We only want to modify HTML responses
		return nil
	}
	wrapped := resp.Body

	// Read in the first kilobyte to see if the <head> tag exists in it
	buf := make([]byte, 1024)
	count, err := wrapped.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	prefix := strings.Replace(string(buf[0:count]), "<head>", "<head>"+webSocketShim, 1)
	resp.Body = &shimmedBody{
		reader: io.MultiReader(strings.NewReader(prefix), wrapped),
		closer: wrapped,
	}
	return nil
}

type websocketConnection struct {
	ctx            context.Context
	cancel         context.CancelFunc
	clientMessages chan string
	ServerMessages chan string
}

func newWebsocketConnection(ctx context.Context, targetURL string, errCallback func(err error)) (*websocketConnection, error) {
	ctx, cancel := context.WithCancel(ctx)
	serverConn, _, err := websocket.DefaultDialer.Dial(targetURL, nil)
	if err != nil {
		log.Printf("Failed to dial the websocket server %q: %v\n", targetURL, err)
		return nil, err
	}
	clientMessages := make(chan string, 10)
	serverMessages := make(chan string, 10)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				close(serverMessages)
				return
			default:
				if _, msgBytes, err := serverConn.ReadMessage(); err != nil {
					errCallback(fmt.Errorf("failed to read a websocket message from the server: %v", err))
				} else {
					serverMessages <- string(msgBytes)
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case clientMsg := <-clientMessages:
				if err := serverConn.WriteMessage(websocket.TextMessage, []byte(clientMsg)); err != nil {
					errCallback(fmt.Errorf("failed to forward websocket data to the server: %v", err))
				}
			}
		}
	}()
	go func() {
		wg.Wait()
		if err := serverConn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
			errCallback(fmt.Errorf("failure issuing a close message on a server websocket connection: %v", err))
		}
		if err := serverConn.Close(); err != nil {
			errCallback(fmt.Errorf("failure closing a server websocket connection: %v", err))
		}
	}()
	return &websocketConnection{
		ctx:            ctx,
		cancel:         cancel,
		clientMessages: clientMessages,
		ServerMessages: serverMessages,
	}, nil
}

func (conn *websocketConnection) Close() {
	conn.cancel()
	close(conn.clientMessages)
}

func (conn *websocketConnection) SendClientMessage(msg string) error {
	select {
	case <-conn.ctx.Done():
		return fmt.Errorf("attempt to send a client message on a closed websocket connection")
	default:
		conn.clientMessages <- msg
	}
	return nil
}

type sessionMessage struct {
	ID      string `json:"id,omitempty"`
	Message string `json:"msg,omitempty"`
}

func createShimChannel(ctx context.Context, shimPath string) http.Handler {
	var connections sync.Map
	var sessionCount uint64
	mux := http.NewServeMux()
	mux.HandleFunc("/"+shimPath+"/open", func(w http.ResponseWriter, r *http.Request) {
		sessionID := fmt.Sprintf("%d", atomic.AddUint64(&sessionCount, 1))
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), http.StatusInternalServerError)
			return
		}
		targetURL, err := url.Parse(string(body))
		if err != nil {
			http.Error(w, fmt.Sprintf("malformed shim open request: %v", err), http.StatusBadRequest)
			return
		}
		targetURL.Scheme = "ws"
		targetURL.Host = *host
		conn, err := newWebsocketConnection(ctx, targetURL.String(),
			func(err error) {
				log.Printf("Websocket failure: %v", err)
			})
		if err != nil {
			log.Printf("Failed to dial the websocket server %q: %v\n", targetURL.String(), err)
			http.Error(w, fmt.Sprintf("internal error opening a shim connection: %v", err), http.StatusInternalServerError)
			return
		}
		connections.Store(sessionID, conn)
		log.Printf("Websocket connection to the server %q established for session: %v\n", targetURL.String(), sessionID)
		resp := &sessionMessage{
			ID:      sessionID,
			Message: targetURL.String(),
		}
		respBytes, err := json.Marshal(resp)
		if err != nil {
			log.Printf("Failed to serialize the response to a websocket open request: %v", err)
			http.Error(w, fmt.Sprintf("internal error opening a shim connection: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	})
	mux.HandleFunc("/"+shimPath+"/close", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), http.StatusInternalServerError)
			return
		}
		var msg sessionMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, fmt.Sprintf("error parsing a shim request: %v", err), http.StatusBadRequest)
			return
		}
		c, ok := connections.Load(msg.ID)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown shim session ID: %q", msg.ID), http.StatusBadRequest)
			return
		}
		conn, ok := c.(*websocketConnection)
		if !ok {
			http.Error(w, "internal error reading a shim session", http.StatusInternalServerError)
			return
		}
		connections.Delete(msg.ID)
		conn.Close()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/"+shimPath+"/data", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), http.StatusInternalServerError)
			return
		}
		var msg sessionMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, fmt.Sprintf("error parsing a shim request: %v", err), http.StatusBadRequest)
			return
		}
		c, ok := connections.Load(msg.ID)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown shim session ID: %q", msg.ID), http.StatusBadRequest)
			return
		}
		conn, ok := c.(*websocketConnection)
		if !ok {
			http.Error(w, "internal error reading a shim session", http.StatusInternalServerError)
			return
		}
		if err := conn.SendClientMessage(msg.Message); err != nil {
			http.Error(w, fmt.Sprintf("attempt to send data on a closed session: %q", msg.ID), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/"+shimPath+"/poll", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), http.StatusInternalServerError)
			return
		}
		var msg sessionMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, fmt.Sprintf("error parsing a shim request: %v", err), http.StatusBadRequest)
			return
		}
		c, ok := connections.Load(msg.ID)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown shim session ID: %q", msg.ID), http.StatusBadRequest)
			return
		}
		conn, ok := c.(*websocketConnection)
		if !ok {
			http.Error(w, "internal error reading a shim session", http.StatusInternalServerError)
			return
		}
		select {
		case serverMsg := <-conn.ServerMessages:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(serverMsg))
		case <-time.After(time.Second * 20):
			w.WriteHeader(http.StatusRequestTimeout)
		}
	})
	return mux
}

func getReverseProxy(ctx context.Context) (http.Handler, error) {
	reverseProxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "http",
		Host:   *host,
	})
	reverseProxy.FlushInterval = 100 * time.Millisecond
	if !*shimWebsockets {
		return reverseProxy, nil
	}

	var templateBuf bytes.Buffer
	if err := websocketShimTmpl.Execute(&templateBuf, &struct{ ShimPath string }{ShimPath: *shimPath}); err != nil {
		return nil, err
	}
	websocketShim := templateBuf.String()
	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		return shimBody(resp, websocketShim)
	}
	shimServer := createShimChannel(ctx, *shimPath)
	mux := http.NewServeMux()
	mux.Handle("/"+*shimPath+"/", shimServer)
	mux.Handle("/", reverseProxy)
	return mux, nil
}

// forwardRequest forwards the given request from the proxy to
// the backend server and reports the response back to the proxy.
func forwardRequest(client *http.Client, reverseProxy http.Handler, request *utils.ForwardedRequest) error {
	httpRequest := request.Contents
	if *forwardUserID {
		httpRequest.Header.Add(utils.HeaderUserID, request.User)
	}
	responseForwarder, err := utils.NewResponseForwarder(client, *proxy, request.BackendID, request.RequestID)
	if err != nil {
		return fmt.Errorf("failed to create the response forwarder: %v", err)
	}
	reverseProxy.ServeHTTP(responseForwarder, httpRequest)
	if *debug {
		log.Printf("Backend latency for request %s: %s\n", request.RequestID, time.Since(request.StartTime).String())
	}
	if err := responseForwarder.Close(); err != nil {
		return fmt.Errorf("failed to close the response forwarder: %v", err)
	}
	return nil
}

// healthCheck issues a health check against the backend server
// and returns the result.
func healthCheck() error {
	resp, err := http.Get("http://" + *host + *healthCheckPath)
	if err != nil {
		log.Printf("Health Check request failed: %s", err.Error())
		return err
	}
	if resp.StatusCode != 200 {
		log.Printf("Health Check request had non-200 status code: %d", resp.StatusCode)
		return fmt.Errorf("Bad Health Check Response Code: %s", resp.Status)
	}
	return nil
}

// processOneRequest reads a single request from the proxy and forwards it to the backend server.
func processOneRequest(client *http.Client, reverseProxy http.Handler, backendID string, requestID string) {
	requestForwarder := func(client *http.Client, request *utils.ForwardedRequest) error {
		if err := forwardRequest(client, reverseProxy, request); err != nil {
			log.Printf("Failure forwarding a request: [%s] %q\n", requestID, err.Error())
			return fmt.Errorf("failed to forward the request %q: %v", requestID, err)
		}
		return nil
	}
	if err := utils.ReadRequest(client, *proxy, backendID, requestID, requestForwarder); err != nil {
		log.Printf("Failed to forward a request: [%s] %q\n", requestID, err.Error())
	}
}

func exponentialBackoffDuration(retryCount uint) time.Duration {
	targetDuration := (1 << retryCount) * time.Millisecond
	if targetDuration > maxBackoffDuration {
		return maxBackoffDuration
	}
	return targetDuration
}

// pollForNewRequests repeatedly reaches out to the proxy server to ask if any pending are available, and then
// processes any newly-seen ones.
func pollForNewRequests(client *http.Client, reverseProxy http.Handler, backendID string) {
	previouslySeenRequests := lru.New(requestCacheLimit)
	var retryCount uint
	for {
		if requests, err := utils.ListPendingRequests(client, *proxy, backendID); err != nil {
			log.Printf("Failed to read pending requests: %q\n", err.Error())
			time.Sleep(exponentialBackoffDuration(retryCount))
			retryCount++
		} else {
			retryCount = 0
			for _, requestID := range requests {
				if _, ok := previouslySeenRequests.Get(requestID); !ok {
					previouslySeenRequests.Add(requestID, requestID)
					go processOneRequest(client, reverseProxy, backendID, requestID)
				}
			}
		}
	}
}

func getGoogleClient(ctx context.Context) (*http.Client, error) {
	sdkConfig, err := google.NewSDKConfig("")
	if err == nil {
		return sdkConfig.Client(ctx), nil
	}

	return google.DefaultClient(ctx, compute.CloudPlatformScope, emailScope)
}

// waitForHealthy runs health checks against the backend and returns
// the first time it sees a healthy check.
func waitForHealthy() {
	if *healthCheckFreq <= 0 {
		return
	}
	if healthCheck() == nil {
		return
	}
	ticker := time.NewTicker(time.Duration(*healthCheckFreq) * time.Second)
	for _ = range ticker.C {
		if healthCheck() == nil {
			ticker.Stop()
			return
		}
	}
}

// runHealthChecks runs health checks against the backend and shuts down
// the proxy if the backend is unhealthy.
func runHealthChecks() {
	if *healthCheckFreq <= 0 {
		return
	}
	if *healthCheckUnhealthy < 1 {
		*healthCheckUnhealthy = 1
	}
	// Always start in the unhealthy state, but only require a single positive
	// health check to become healthy for the first time, and do the first check
	// immediately.
	ticker := time.NewTicker(time.Duration(*healthCheckFreq) * time.Second)
	badHealthChecks := 0
	for _ = range ticker.C {
		if healthCheck() != nil {
			badHealthChecks++
		} else {
			badHealthChecks = 0
		}
		if badHealthChecks >= *healthCheckUnhealthy {
			ticker.Stop()
			log.Fatal("Too many unhealthy checks")
		}
	}
}

// runAdapter sets up the HTTP client for the agent to use (including OAuth credentials),
// and then does the actual work of forwarding requests and responses.
func runAdapter() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := getGoogleClient(ctx)
	if err != nil {
		return err
	}

	reverseProxy, err := getReverseProxy(ctx)
	if err != nil {
		return err
	}
	pollForNewRequests(client, reverseProxy, *backendID)
	return nil
}

func main() {
	flag.Parse()

	if *proxy == "" {
		log.Fatal("You must specify the address of the proxy")
	}
	if *backendID == "" {
		log.Fatal("You must specify a backend ID")
	}
	if !strings.HasPrefix(*healthCheckPath, "/") {
		*healthCheckPath = "/" + *healthCheckPath
	}

	waitForHealthy()
	go runHealthChecks()

	if err := runAdapter(); err != nil {
		log.Fatal(err.Error())
	}
}

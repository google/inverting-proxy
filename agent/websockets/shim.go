/*
Copyright 2018 Google Inc. All rights reserved.

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

package websockets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"

	"context"

	"github.com/google/inverting-proxy/agent/metrics"
)

const (
	contentTypeHeader = "Content-Type"
	shimTemplate      = `
<!--START_WEBSOCKET_SHIM-->
<!--
    This file was served from behind a reverse proxy that does not support websockets.

    The following code snippet has been inserted by the proxy to replace websockets
    with HTTP requests that will work with this proxy.

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
      function receiveHandler(resp) {
        var msgs = JSON.parse(resp);
        if (self.onmessage) {
          msgs.forEach(function(msg) {
            if (Array.isArray(msg)) {
              if (self.protocolVersion != 0) {
                for (var i = 0; i < msg.length; i++) {
                    msg[i] == atob(msg[i]);
                }
              }
              
              msg = new Blob(msg);
            }
            self.onmessage({ target: self, data: msg });
          });
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
        req.setRequestHeader("X-Websocket-Shim-Version", "1");
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

      self.pendingMessages = [];
      self.needsConversion = function(msg) {
        if (typeof msg == 'string') {
          return false;
        }
        if (Array.isArray(msg)) {
          if (msg.length == 1) {
            if (typeof msg[0] == 'string') {
              return false;
            }
          }
        }
        return true;
      }
      self.convertMessagesAndPush = function(msgs) {
        for (var i = 0; i < msgs.length; i++) {
          if (self.needsConversion(msgs[i].msg)) {
            var blob = new Blob([msgs[i].msg]);
            var reader = new FileReader();
            reader.addEventListener("loadend", function() {
            if (self.protocolVersion != 0 ) {
                msgs[i].msg = [btoa(reader.result)];
            } else {
                msgs[i].msg = [reader.result];
            }
            self.convertMessagesAndPush(msgs);
            });
            reader.readAsText(blob);
            return;
          }
        }
        self.xhr('data', msgs, null, function() {
          self.pushing = false;
          self.push();
        });
      }
      self.push = function() {
         if (self.pushing) {
           return;
         }
         if (self.pendingMessages.length == 0) {
           return;
         }
         self.pushing = true;
         var msgs = self.pendingMessages;
         self.pendingMessages = [];
         self.convertMessagesAndPush(msgs);
      }

      function poll() {
        if (self.readyState != WebSocketShim.OPEN) {
          return;
        }
        self.xhr('poll', {'id': self._sessionID}, receiveHandler, poll);
      }

      self.xhr('open', url, function(resp) {
        respJSON = JSON.parse(resp);
        if (respJSON.v === undefined) {
            self.protocolVersion = 0;
        } else {
            self.protocolVersion = respJSON.v;
        }
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
        this.pendingMessages.push({'id': this._sessionID, 'msg': data});
        this.push();
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

var shimTmpl = template.Must(template.New("client-shim").Parse(shimTemplate))

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

// ShimBody returns a function that injects code into a *http.Response Body
func ShimBody(shimPath string) (func(resp *http.Response) error, error) {
	var templateBuf bytes.Buffer
	if err := shimTmpl.Execute(&templateBuf, &struct{ ShimPath string }{ShimPath: shimPath}); err != nil {
		return nil, err
	}
	shimCode := templateBuf.String()
	return func(resp *http.Response) error {
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
		prefix := strings.Replace(string(buf[0:count]), "<head>", "<head>"+shimCode, 1)
		resp.Body = &shimmedBody{
			reader: io.MultiReader(strings.NewReader(prefix), wrapped),
			closer: wrapped,
		}
		resp.Header.Del("Content-Length")
		return nil
	}, nil
}

type sessionMessage struct {
	ID      string      `json:"id,omitempty"`
	Message interface{} `json:"msg,omitempty"`
	Version int         `json:"v,omitempty"`
}

func createShimChannel(ctx context.Context, host, shimPath string, rewriteHost bool, openWebsocketWrapper func(http.Handler, *metrics.MetricHandler) http.Handler, enableWebsocketInjection bool, metricHandler *metrics.MetricHandler) http.Handler {
	var connections sync.Map
	var sessionCount uint64
	mux := http.NewServeMux()
	openWebsocketHandler := openWebsocketWrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID := fmt.Sprintf("%d", atomic.AddUint64(&sessionCount, 1))
		targetURL := *(r.URL)
		targetURL.Scheme = "ws"
		targetURL.Host = host
		if originalHost := r.Host; rewriteHost && originalHost != "" {
			r.Header.Set("Host", originalHost)
		}
		conn, err := NewConnection(ctx, targetURL.String(), r.Header,
			func(err error) {
				log.Printf("Websocket failure: %v", err)
			})
		if err != nil {
			log.Printf("Failed to dial the websocket server %q: %v\n", targetURL.String(), err)
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error opening a shim connection: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		connections.Store(sessionID, conn)
		log.Printf("Websocket connection to the server %q established for session: %v\n", targetURL.String(), sessionID)
		vh := r.Header.Get("X-Websocket-Shim-Version")
		if vh != "" {
			v, err := strconv.ParseInt(vh, 10, 64)
			if err == nil {
				conn.protocolVersion = int(v)
			}
		}
		resp := &sessionMessage{
			ID:      sessionID,
			Message: targetURL.String(),
			Version: conn.protocolVersion,
		}
		respBytes, err := json.Marshal(resp)
		if err != nil {
			log.Printf("Failed to serialize the response to a websocket open request: %v", err)
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error opening a shim connection: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		statusCode := http.StatusOK
		w.WriteHeader(statusCode)
		metricHandler.WriteResponseCodeMetric(statusCode)
		w.Write(respBytes)
	}), metricHandler)
	mux.HandleFunc(path.Join(shimPath, "open"), func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		targetURL, err := url.Parse(string(body))
		if err != nil {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("malformed shim open request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		// Restore the original request URL before calling the openWebsocketWrapper
		r.URL = targetURL
		openWebsocketHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc(path.Join(shimPath, "close"), func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		var msg sessionMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("error parsing a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		c, ok := connections.Load(msg.ID)
		if !ok {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("unknown shim session ID: %q", msg.ID), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		conn, ok := c.(*Connection)
		if !ok {
			statusCode := http.StatusInternalServerError
			http.Error(w, "internal error reading a shim session", statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		connections.Delete(msg.ID)
		conn.Close()
		statusCode := http.StatusOK
		w.WriteHeader(statusCode)
		w.Write([]byte("ok"))
		metricHandler.WriteResponseCodeMetric(statusCode)
	})
	mux.HandleFunc(path.Join(shimPath, "data"), func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		var msgs []sessionMessage
		if err := json.Unmarshal(body, &msgs); err != nil {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("error parsing a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		var injectedHeaders map[string]string
		if enableWebsocketInjection {
			injectedHeaders = map[string]string{}
			for key, value := range r.Header {
				if len(value) > 0 {
					// only grab the first header value
					injectedHeaders[key] = value[0]
					if len(value) > 1 {
						log.Printf("More than one header value found for header %q; silently dropping...", key)
					}
				}
			}
		}
		for _, msg := range msgs {
			c, ok := connections.Load(msg.ID)
			if !ok {
				statusCode := http.StatusBadRequest
				http.Error(w, fmt.Sprintf("unknown shim session ID: %q", msg.ID), statusCode)
				metricHandler.WriteResponseCodeMetric(statusCode)
				return
			}
			conn, ok := c.(*Connection)
			if !ok {
				statusCode := http.StatusInternalServerError
				http.Error(w, "internal error reading a shim session", statusCode)
				metricHandler.WriteResponseCodeMetric(statusCode)
				return
			}
			if err := conn.SendClientMessage(msg.Message, enableWebsocketInjection, injectedHeaders); err != nil {
				statusCode := http.StatusBadRequest
				http.Error(w, fmt.Sprintf("attempt to send data on a closed session: %q", msg.ID), statusCode)
				metricHandler.WriteResponseCodeMetric(statusCode)
				return
			}
		}
		statusCode := http.StatusOK
		w.WriteHeader(statusCode)
		w.Write([]byte("ok"))
		metricHandler.WriteResponseCodeMetric(statusCode)
	})
	mux.HandleFunc(path.Join(shimPath, "poll"), func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error reading a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		var msg sessionMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("error parsing a shim request: %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		c, ok := connections.Load(msg.ID)
		if !ok {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("unknown shim session ID: %q", msg.ID), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		conn, ok := c.(*Connection)
		if !ok {
			statusCode := http.StatusInternalServerError
			http.Error(w, "internal error reading a shim session", statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		serverMsgs, err := conn.ReadServerMessages()
		if err != nil {
			statusCode := http.StatusBadRequest
			http.Error(w, fmt.Sprintf("attempt to read data from a closed session: %q", msg.ID), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		} else if serverMsgs == nil {
			statusCode := http.StatusRequestTimeout
			w.WriteHeader(statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		respBytes, err := json.Marshal(serverMsgs)
		if err != nil {
			statusCode := http.StatusInternalServerError
			http.Error(w, fmt.Sprintf("internal error serializing the server-side messages %v", err), statusCode)
			metricHandler.WriteResponseCodeMetric(statusCode)
			return
		}
		statusCode := http.StatusOK
		w.WriteHeader(statusCode)
		w.Write(respBytes)
		metricHandler.WriteResponseCodeMetric(statusCode)
	})
	return mux
}

// Proxy creates a reverse proxy that inserts websocket-shim code into all HTML responses.
// openWebsocketWrapper is a http.Handler wrapper function that is invoked on websocket open requests after the original
// targetURL of the request is restored. It must call the wrapped http.Handler with which it is created after it
// is finished processing the request.
func Proxy(ctx context.Context, wrapped http.Handler, host, shimPath string, rewriteHost, enableWebsocketInjection bool, openWebsocketWrapper func(wrapped http.Handler, metricHandler *metrics.MetricHandler) http.Handler, metricHandler *metrics.MetricHandler) (http.Handler, error) {
	mux := http.NewServeMux()
	if shimPath != "" {
		shimPath = path.Clean("/"+shimPath) + "/"
		shimServer := createShimChannel(ctx, host, shimPath, rewriteHost, openWebsocketWrapper, enableWebsocketInjection, metricHandler)
		mux.Handle(shimPath, shimServer)
	}
	mux.Handle("/", wrapped)
	return mux, nil
}

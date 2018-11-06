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

// Package websockets defines logic for the agent routing websocket connections.
package websockets

import (
	"bytes"
	"encoding/json"
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

	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
)

const (
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

var websocketShimTmpl = template.Must(template.New("client-shim").Parse(websocketShimTemplate))

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

func createShimChannel(ctx context.Context, host, shimPath string) http.Handler {
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
		targetURL.Host = host
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

func ReverseProxy(ctx context.Context, wrapped *httputil.ReverseProxy, host, shimPath string) (http.Handler, error) {
	var templateBuf bytes.Buffer
	if err := websocketShimTmpl.Execute(&templateBuf, &struct{ ShimPath string }{ShimPath: shimPath}); err != nil {
		return nil, err
	}
	websocketShim := templateBuf.String()
	wrapped.ModifyResponse = func(resp *http.Response) error {
		return shimBody(resp, websocketShim)
	}
	shimServer := createShimChannel(ctx, host, shimPath)
	mux := http.NewServeMux()
	mux.Handle("/"+shimPath+"/", shimServer)
	mux.Handle("/", wrapped)
	return mux, nil
}

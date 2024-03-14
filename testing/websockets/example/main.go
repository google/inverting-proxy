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

// Command example launches a server that can be used to exercise the
// different websocket features. This, in turn, is used to test the
// implementation of websockets provided by the inverting proxy and agent.
//
// Example usage:
//
//	go build -o ~/bin/example-websocket-sever testing/websockets/example/main.go
//	~/bin/example-websocket-server --port 8082
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

const (
	subProtocol1 = "Sub-Protocol-1"
	subProtocol2 = "Sub-Protocol-2"
	subProtocol3 = "Sub-Protocol-3"
	subProtocol4 = "Sub-Protocol-4"

	html = `<!DOCTYPE html>
<html>
  <head>
    <title>Example websocket server</title>
  </head>
  <body>
    <div id="log"/>
    <script>
    const logElement = document.getElementById("log");
    function logEvent(msg, e) {
        const entry = document.createElement("div");
        entry.innerHTML = msg + " " + JSON.stringify(e);
        logElement.appendChild(entry);
    }

    const conn = new WebSocket("ws://" + document.location.host + "/ws");
    conn.onopen = function(e) {
      logEvent("Connection opened [" + conn.protocol + "(" + conn.binaryType + ")]", e);
    };
    conn.onclose = function(e) {
      logEvent("Connection closed", e);
    };
    conn.onmessage = function(e) {
      var messageType = (typeof e.data);
      logEvent("Message received [" + e.lastEventId + "(" + messageType + ")]", e);
    };

    function sendMessages(){
      conn.send("Text Message");
	  conn.send(new Blob(["Binary Message"]));
	  conn.send("{\"message\":\"JSON text message\",\"resource\":{\"headers\":{}}}");
	  conn.send(new Blob(["{\"message\":\"JSON binary message\",\"resource\":{\"headers\":{}}}"]));
    }
    window.setInterval(sendMessages, 10000);
    </script>
  </body>
</html>
`
)

var (
	port = flag.Int("port", 0, "Port on which to listen")

	upgrader = websocket.Upgrader{
		Subprotocols: []string{
			subProtocol1,
			subProtocol2,
			subProtocol3,
			subProtocol4,
		},
	}
)

func homeServer(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(html))
	return
}

type message struct {
	Type int
	Data []byte
}

func wsServer(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, fmt.Sprintf("Unsupported request: %+v", r), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Unexpected server error when upgrading: %+v\n", err)
		http.Error(w, fmt.Sprintf("Unexpected server error: %v", err), http.StatusInternalServerError)
		return
	}
	conn.SetCloseHandler(func(code int, text string) error {
		cancel()
		return nil
	})
	defer conn.Close()
	msgChan := make(chan *message, 1024)
	go func(ctx context.Context, conn *websocket.Conn, msgChan chan *message) {
		for {
			mt, mb, err := conn.ReadMessage()
			if err != nil {
				select {
				case <-ctx.Done():
				// The connection was closed, so this does not warrant logging...
				default:
					// The error is something else, so log it...
					log.Printf("Error reading a message on a connection: %v\n", err)
				}
				// All errors on a connection are terminal
				return
			}
			select {
			case <-ctx.Done():
				return
			case msgChan <- &message{Type: mt, Data: mb}:
				continue
			}
		}
	}(ctx, conn, msgChan)
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-msgChan:
			err := conn.WriteMessage(m.Type, m.Data)
			if err != nil {
				log.Printf("Error writing a message back on a connection: %v\n", err)
				return
			}
		}
	}
}

func main() {
	flag.Parse()
	if *port == 0 {
		log.Fatal("You must specify a local port number on which to listen")
	}
	http.HandleFunc("/", homeServer)
	http.HandleFunc("/ws", wsServer)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

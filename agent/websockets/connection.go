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
	"fmt"
	"net/http"
	"sync"
	"time"

	"context"
	"github.com/gorilla/websocket"
)

// Connection implements a websocket client connection.
//
// This wraps a websocket.Conn connection as defined by the gorilla/websocket library,
// and encapsulates it in an API that is a little more amenable to how the server side
// of our websocket shim is implemented.
type Connection struct {
	ctx            context.Context
	cancel         context.CancelFunc
	clientMessages chan string
	serverMessages chan string
}

// This map defines the set of headers that should be stripped from the WS request, as they
// are being verified by the websocket.Client.
var stripHeaderNames = map[string]bool{
	"Upgrade":                  true,
	"Connection":               true,
	"Sec-Websocket-Key":        true,
	"Sec-Websocket-Version":    true,
	"Sec-Websocket-Extensions": true,
	"Sec-Websocket-Protocol":   true,
}

// stripWSHeader strips request headers that are not allowed by the websocket.Client library,
// while preserving the rest.
func stripWSHeader(header http.Header) http.Header {
	result := http.Header{}
	for k, vals := range header {
		if !stripHeaderNames[k] {
			for _, v := range vals {
				result.Add(k, v)
			}
		}
	}
	return result
}

// NewConnection creates and returns a new Connection.
func NewConnection(ctx context.Context, targetURL string, header http.Header, errCallback func(err error)) (*Connection, error) {
	ctx, cancel := context.WithCancel(ctx)
	serverConn, _, err := websocket.DefaultDialer.Dial(targetURL, stripWSHeader(header))
	if err != nil {
		cancel()
		return nil, err
	}

	// The underlying websocket library assumes that clients are written to read from the connection
	// in a tight loop. We prefer to instead have the ability to poll for the next message with a timeout.
	//
	// The most direct way to bridge those two approaches is to spawn a goroutine that reads in a
	// tight loop, and then sends the read messages in to an open channel.
	serverMessages := make(chan string, 10)

	// Since we are using a channel to pull messages from the connection, we will also use one to
	// push messages. That way our handling of reads and writes are consistent.
	clientMessages := make(chan string, 10)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			close(serverMessages)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, msgBytes, err := serverConn.ReadMessage()
				if err != nil {
					errCallback(fmt.Errorf("failed to read a websocket message from the server: %v", err))
					// Errors from the server connection are terminal; once an error is returned
					// no subsequent calls will succeed.
					return
				}
				serverMessages <- string(msgBytes)
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
					// Errors writing to the server connection are terminal; once an error is returned
					// no subsequent calls will succeed.
					return
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
	return &Connection{
		ctx:            ctx,
		cancel:         cancel,
		clientMessages: clientMessages,
		serverMessages: serverMessages,
	}, nil
}

// Close closes the websocket client connection.
func (conn *Connection) Close() {
	conn.cancel()
	close(conn.clientMessages)
}

// SendClientMessage sends the given message to the websocket server.
//
// The returned error value is non-nill if the connection has been closed.
func (conn *Connection) SendClientMessage(msg string) error {
	select {
	case <-conn.ctx.Done():
		return fmt.Errorf("attempt to send a client message on a closed websocket connection")
	default:
		conn.clientMessages <- msg
	}
	return nil
}

// ReadServerMessages reads the next messages from the websocket server.
//
// The returned error value is non-nill if the connection has been closed.
//
// The returned []string value is nil if the error is non-nil, or if the method
// times out while waiting for a server message.
func (conn *Connection) ReadServerMessages() ([]string, error) {
	var msgs []string
	select {
	case serverMsg, ok := <-conn.serverMessages:
		if !ok {
			// The server messages channel has been closed.
			return nil, fmt.Errorf("attempt to read a server message from a closed websocket connection")
		}
		msgs = append(msgs, serverMsg)
		for {
			select {
			case serverMsg, ok := <-conn.serverMessages:
				if ok {
					msgs = append(msgs, serverMsg)
				} else {
					return msgs, nil
				}
			default:
				return msgs, nil
			}
		}
	case <-time.After(time.Second * 20):
		return nil, nil
	}
}

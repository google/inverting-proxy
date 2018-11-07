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
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
)

type Connection struct {
	ctx            context.Context
	cancel         context.CancelFunc
	clientMessages chan string
	serverMessages chan string
}

func NewConnection(ctx context.Context, targetURL string, errCallback func(err error)) (*Connection, error) {
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
		defer func() {
			close(serverMessages)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if _, msgBytes, err := serverConn.ReadMessage(); err != nil {
					errCallback(fmt.Errorf("failed to read a websocket message from the server: %v", err))
					// Errors from the server connection are terminal; once an error is returned
					// no subsequent calls will succeed.
					return
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

func (conn *Connection) Close() {
	conn.cancel()
	close(conn.clientMessages)
}

func (conn *Connection) SendClientMessage(msg string) error {
	select {
	case <-conn.ctx.Done():
		return fmt.Errorf("attempt to send a client message on a closed websocket connection")
	default:
		conn.clientMessages <- msg
	}
	return nil
}

func (conn *Connection) ReadServerMessage() (*string, error) {
	select {
	case serverMsg, ok := <-conn.serverMessages:
		if !ok {
			// The server messages channel has been closed.
			return nil, fmt.Errorf("attempt to read a server message from a closed websocket connection")
		}
		return &serverMsg, nil
	case <-time.After(time.Second * 20):
		return nil, nil
	}
}

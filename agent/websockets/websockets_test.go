/*
Copyright 2020 Google Inc. All rights reserved.

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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/gorilla/websocket"

	"github.com/google/inverting-proxy/agent/metrics"
)

func TestInjectWebsocketMessage(t *testing.T) {
	// Create the request bytes to use in the test cases
	simpleMsgStruct := &map[string]interface{}{
		"test": &map[string]interface{}{},
	}
	complexMsgStruct := &map[string]interface{}{
		"a": "y",
		"b": &map[string]interface{}{
			"aa": false,
			"ab": map[string]string{"q": "r"},
		},
		"c": "z",
	}
	simpleMsgBytes, err := json.Marshal(simpleMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	complexMsgBytes, err := json.Marshal(&complexMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	emptyMsg := &message{
		Type: websocket.TextMessage,
		Data: []byte("{}"),
	}
	invalidEmptyMsg := &message{
		Type: websocket.TextMessage,
		Data: []byte(""),
	}
	simpleMsg := &message{
		Type: websocket.TextMessage,
		Data: simpleMsgBytes,
	}
	complexMsg := &message{
		Type: websocket.TextMessage,
		Data: complexMsgBytes,
	}
	invalidFormatMsg := &message{
		Type: websocket.TextMessage,
		Data: []byte("invalid"),
	}

	testCases := []struct {
		description     string
		message         *message
		injectionPath   []string
		injectionValues map[string]string
		wantMessage     []byte
		wantError       error
	}{
		{
			description:     "Invalid empty websocket message returns error",
			message:         invalidEmptyMsg,
			injectionPath:   []string{},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:     "Valid empty websocket message injects one value",
			message:         emptyMsg,
			injectionPath:   []string{},
			injectionValues: map[string]string{"test": "value"},
			wantMessage:     []byte("{\"test\":\"value\"}"),
		},
		{
			description:     "Valid simple websocket message injects one value",
			message:         simpleMsg,
			injectionPath:   []string{"test"},
			injectionValues: map[string]string{"test_key": "value"},
			wantMessage:     []byte("{\"test\":{\"test_key\":\"value\"}}"),
		},
		{
			description:     "Valid simple websocket message with no path component returns error",
			message:         simpleMsg,
			injectionPath:   []string{"does_not_exist"},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:     "Valid complex websocket message injects one value",
			message:         complexMsg,
			injectionPath:   []string{"b", "ab"},
			injectionValues: map[string]string{"test": "value"},
			wantMessage:     []byte("{\"a\":\"y\",\"b\":{\"aa\":false,\"ab\":{\"q\":\"r\",\"test\":\"value\"}},\"c\":\"z\"}"),
		},
		{
			description:     "Valid complex websocket message with no path component object returns error",
			message:         complexMsg,
			injectionPath:   []string{"b", "does_not_exist"},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:     "Nil message returns error",
			message:         nil,
			injectionPath:   []string{"b", "ab"},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:     "Non JSON message injects returns error",
			message:         invalidFormatMsg,
			injectionPath:   []string{"b", "ab"},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:   "Message with nil values returns no error",
			message:       simpleMsg,
			injectionPath: []string{"test"},
			wantMessage:   []byte("{\"test\":{}}"),
		},
		{
			description:     "Message with empty values injects no values and returns no errors",
			message:         simpleMsg,
			injectionPath:   []string{"test"},
			injectionValues: map[string]string{},
			wantMessage:     []byte("{\"test\":{}}"),
		},
		{
			description:   "Complex message with multiple values injects multiple values",
			message:       complexMsg,
			injectionPath: []string{"b", "ab"},
			injectionValues: map[string]string{
				"test1": "value1",
				"test2": "value2",
				"test3": "value3",
			},
			wantMessage: []byte("{\"a\":\"y\",\"b\":{\"aa\":false,\"ab\":{\"q\":\"r\",\"test1\":\"value1\",\"test2\":\"value2\",\"test3\":\"value3\"}},\"c\":\"z\"}"),
		},
		{
			description:   "Complex message with multiple values injects values without overwriting initial values",
			message:       complexMsg,
			injectionPath: []string{"b", "ab"},
			injectionValues: map[string]string{
				"q":     "value1",
				"test2": "value2",
				"test3": "value3",
			},
			wantMessage: []byte("{\"a\":\"y\",\"b\":{\"aa\":false,\"ab\":{\"q\":\"r\",\"test2\":\"value2\",\"test3\":\"value3\"}},\"c\":\"z\"}"),
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.description, func(t *testing.T) {
			t.Parallel()
			injectedMsg, err := injectWebsocketMessage(testCase.message, testCase.injectionPath, testCase.injectionValues)
			if got, want := err, testCase.wantError; !cmp.Equal(got, want, cmpopts.EquateErrors()) {
				t.Errorf("injectWebsocketMessage(%+v, %+v, %+v): Unexpected error: got %v, want %v", testCase.message, testCase.injectionPath, testCase.injectionValues, got, want)
			}
			if err == nil { // if NO error
				if got, want := injectedMsg.Data, testCase.wantMessage; !cmp.Equal(got, want, cmpopts.EquateEmpty()) {
					t.Errorf("injectWebsocketMessage(%+v, %+v, %q): Unexpected message bytes: got %q, want %q", testCase.message, testCase.injectionPath, testCase.injectionValues, got, want)
				}
			}
		})
	}
}

func TestShimHandlers(t *testing.T) {
	testShimPath := "/websocket-shim"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "only websocket connections are supported", http.StatusBadRequest)
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, http.Header{})
		if err != nil {
			t.Fatalf("Failure upgrading the websocket connection: %+v", err)
		}
		defer conn.Close()
		for {
			messageType, msg, err := conn.ReadMessage()
			var closeError *websocket.CloseError
			if errors.As(err, &closeError) && closeError.Code == websocket.CloseNormalClosure {
				return
			}
			if err != nil {
				t.Logf("Error returned from ReadMessage: %+v", err)
				return
			}
			if err := conn.WriteMessage(messageType, msg); err != nil {
				t.Logf("Error returned from WriteMessage: %+v", err)
				return
			}
		}
	})
	server := httptest.NewServer(h)
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	openWrapper := func(h http.Handler, metricHandler *metrics.MetricHandler) http.Handler {
		return h
	}
	p, err := Proxy(context.Background(), h, serverURL.Host, testShimPath, false, false, openWrapper, nil)
	if err != nil {
		t.Fatalf("Failure creating the websocket shim proxy: %+v", err)
	}
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()

	openConnection := func() (string, error) {
		openResp, err := proxyServer.Client().Post(proxyServer.URL+path.Join(testShimPath, "open"), "", strings.NewReader(server.URL))
		if err != nil {
			return "", fmt.Errorf("failure opening the shimmed websocket connection: %+v", err)
		}
		defer openResp.Body.Close()
		respBytes, err := io.ReadAll(openResp.Body)
		if err != nil {
			return "", fmt.Errorf("failure reading the open response: %+v", err)
		}
		var openedSession sessionMessage
		if err := json.Unmarshal(respBytes, &openedSession); err != nil {
			return "", fmt.Errorf("failure parsing the open response: %+v", err)
		}
		return openedSession.ID, nil
	}

	closeConnection := func(sessionID string) error {
		closeMessage := &sessionMessage{
			ID: sessionID,
		}
		closeBytes, err := json.Marshal(closeMessage)
		if err != nil {
			return fmt.Errorf("failure marshalling the close session message: %+v", err)
		}
		closeResp, err := proxyServer.Client().Post(proxyServer.URL+path.Join(testShimPath, "close"), "", bytes.NewReader(closeBytes))
		if err != nil {
			return fmt.Errorf("failure closing the shimmed websocket connection: %+v", err)
		}
		defer closeResp.Body.Close()
		if got, want := closeResp.StatusCode, http.StatusOK; got != want {
			return fmt.Errorf("unexpected close response status code: got %v, want %v", got, want)
		}
		return nil
	}

	sendMessage := func(sessionID, message string) error {
		dataMessages := []*sessionMessage{
			&sessionMessage{
				ID:      sessionID,
				Message: message,
			},
		}
		dataBytes, err := json.Marshal(dataMessages)
		if err != nil {
			return fmt.Errorf("failure marshalling the data session messages: %+v", err)
		}
		dataResp, err := proxyServer.Client().Post(proxyServer.URL+path.Join(testShimPath, "data"), "", bytes.NewReader(dataBytes))
		if err != nil {
			return fmt.Errorf("failure writing to the shimmed websocket connection: %+v", err)
		}
		defer dataResp.Body.Close()
		if got, want := dataResp.StatusCode, http.StatusOK; got != want {
			return fmt.Errorf("unexpected data response status code: got %v, want %v", got, want)
		}
		return nil
	}

	readMessage := func(sessionID string) (string, error) {
		pollMessage := &sessionMessage{
			ID: sessionID,
		}
		pollBytes, err := json.Marshal(pollMessage)
		if err != nil {
			return "", fmt.Errorf("failure marshalling the poll session messages: %+v", err)
		}
		pollResp, err := proxyServer.Client().Post(proxyServer.URL+path.Join(testShimPath, "poll"), "", bytes.NewReader(pollBytes))
		if err != nil {
			return "", fmt.Errorf("failure reading from the shimmed websocket connection: %+v", err)
		}
		defer pollResp.Body.Close()
		if got, want := pollResp.StatusCode, http.StatusOK; got != want {
			return "", fmt.Errorf("unexpected poll response status code: got %v, want %v", got, want)
		}
		respBytes, err := io.ReadAll(pollResp.Body)
		if err != nil {
			return "", fmt.Errorf("failure reading the poll response: %+v", err)
		}
		var readMessages []string
		if err := json.Unmarshal(respBytes, &readMessages); err != nil {
			return "", fmt.Errorf("failure parsing the poll response: %+v", err)
		}
		if got, want := len(readMessages), 1; got != want {
			return "", fmt.Errorf("unexpected number of websocket messages read; got %d, want %d", got, want)
		}
		return readMessages[0], nil
	}

	for i := 0; i < 100; i++ {
		sessionID, err := openConnection()
		if err != nil {
			t.Errorf("Failure opening the connection: %v", err)
		}
		for j := 0; j < 100; j++ {
			msg := fmt.Sprintf("connection #%d, message #%d", i, j)
			if err := sendMessage(sessionID, msg); err != nil {
				t.Errorf("Failure sending message %q: %v", msg, err)
			} else if readMsg, err := readMessage(sessionID); err != nil {
				t.Errorf("Failure reading back the message %q: %v", msg, err)
			} else if got, want := readMsg, msg; got != want {
				t.Errorf("Unexpected message read back; got %q, want %q", got, want)
			}
		}
		if err := closeConnection(sessionID); err != nil {
			t.Errorf("Failure closing the connection: %v", err)
		}
	}
}

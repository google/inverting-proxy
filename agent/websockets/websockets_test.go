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
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/gorilla/websocket"
)

func TestInjectWebsocketMessage(t *testing.T) {
	// Create the request bytes to use in the test cases
	simpleMsgStruct := map[string]interface{}{
		"test": &map[string]interface{}{},
	}
	complexMsgStruct := map[string]interface{}{
		"a": "y",
		"b": &map[string]interface{}{
			"aa": false,
			"ab": map[string]string{"q": "r"},
		},
		"test": &map[string]interface{}{},
		"c":    "z",
	}
	simpleMsgBytes, err := json.Marshal(simpleMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	complexMsgBytes, err := json.Marshal(complexMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	singleSimpleMsgStruct := []string{string(simpleMsgBytes)}
	singleSimpleMsgBytes, err := json.Marshal(singleSimpleMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	singleComplexMsgStruct := []string{string(complexMsgBytes)}
	singleComplexMsgBytes, err := json.Marshal(singleComplexMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	multipleMsgStruct := []string{string(simpleMsgBytes), string(complexMsgBytes)}
	multipleMsgBytes, err := json.Marshal(multipleMsgStruct)
	if err != nil {
		t.Fatalf("Failed to marshal message: %v", err)
	}
	emptyMsg := &message{
		Type: websocket.TextMessage,
		Data: []byte("[\"{}\"]"),
	}
	invalidEmptyMsg := &message{
		Type: websocket.TextMessage,
		Data: []byte(""),
	}
	simpleMsg := &message{
		Type: websocket.TextMessage,
		Data: singleSimpleMsgBytes,
	}
	complexMsg := &message{
		Type: websocket.TextMessage,
		Data: singleComplexMsgBytes,
	}
	multipleMsg := &message{
		Type: websocket.TextMessage,
		Data: multipleMsgBytes,
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
			description:     "Single valid empty websocket message injects one value",
			message:         emptyMsg,
			injectionPath:   []string{},
			injectionValues: map[string]string{"test": "value"},
			wantMessage:     []byte("[\"{\\\"test\\\":\\\"value\\\"}\"]"),
		},
		{
			description:     "Single valid simple websocket message injects one value",
			message:         simpleMsg,
			injectionPath:   []string{"test"},
			injectionValues: map[string]string{"test_key": "value"},
			wantMessage:     []byte("[\"{\\\"test\\\":{\\\"test_key\\\":\\\"value\\\"}}\"]"),
		},
		{
			description:     "Single valid simple websocket message with no path component returns error",
			message:         simpleMsg,
			injectionPath:   []string{"does_not_exist"},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:     "Single valid complex websocket message injects one value",
			message:         complexMsg,
			injectionPath:   []string{"b", "ab"},
			injectionValues: map[string]string{"test": "value"},
			wantMessage:     []byte("[\"{\\\"a\\\":\\\"y\\\",\\\"b\\\":{\\\"aa\\\":false,\\\"ab\\\":{\\\"q\\\":\\\"r\\\",\\\"test\\\":\\\"value\\\"}},\\\"c\\\":\\\"z\\\",\\\"test\\\":{}}\"]"),
		},
		{
			description:     "Single valid complex websocket message with no path component object returns error",
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
			description:     "Single non JSON message injects returns error",
			message:         invalidFormatMsg,
			injectionPath:   []string{"b", "ab"},
			injectionValues: map[string]string{"test": "value"},
			wantError:       cmpopts.AnyError,
		},
		{
			description:   "Single message with nil values returns no error",
			message:       simpleMsg,
			injectionPath: []string{"test"},
			wantMessage:   []byte("[\"{\\\"test\\\":{}}\"]"),
		},
		{
			description:     "Single message with empty values injects no values and returns no errors",
			message:         simpleMsg,
			injectionPath:   []string{"test"},
			injectionValues: map[string]string{},
			wantMessage:     []byte("[\"{\\\"test\\\":{}}\"]"),
		},
		{
			description:   "Single complex message with multiple values injects multiple values",
			message:       complexMsg,
			injectionPath: []string{"b", "ab"},
			injectionValues: map[string]string{
				"test1": "value1",
				"test2": "value2",
				"test3": "value3",
			},
			wantMessage: []byte("[\"{\\\"a\\\":\\\"y\\\",\\\"b\\\":{\\\"aa\\\":false,\\\"ab\\\":{\\\"q\\\":\\\"r\\\",\\\"test1\\\":\\\"value1\\\",\\\"test2\\\":\\\"value2\\\",\\\"test3\\\":\\\"value3\\\"}},\\\"c\\\":\\\"z\\\",\\\"test\\\":{}}\"]"),
		},
		{
			description:   "Single complex message with multiple values injects values without overwriting initial values",
			message:       complexMsg,
			injectionPath: []string{"b", "ab"},
			injectionValues: map[string]string{
				"q":     "value1",
				"test2": "value2",
				"test3": "value3",
			},
			wantMessage: []byte("[\"{\\\"a\\\":\\\"y\\\",\\\"b\\\":{\\\"aa\\\":false,\\\"ab\\\":{\\\"q\\\":\\\"r\\\",\\\"test2\\\":\\\"value2\\\",\\\"test3\\\":\\\"value3\\\"}},\\\"c\\\":\\\"z\\\",\\\"test\\\":{}}\"]"),
		},
		{
			description:   "Multiple messages injects values into all messages",
			message:       multipleMsg,
			injectionPath: []string{"test"},
			injectionValues: map[string]string{
				"test_key": "value",
			},
			wantMessage: []byte("[\"{\\\"test\\\":{\\\"test_key\\\":\\\"value\\\"}}\",\"{\\\"a\\\":\\\"y\\\",\\\"b\\\":{\\\"aa\\\":false,\\\"ab\\\":{\\\"q\\\":\\\"r\\\"}},\\\"c\\\":\\\"z\\\",\\\"test\\\":{\\\"test_key\\\":\\\"value\\\"}}\"]"),
		},
		{
			description:   "Multiple messages with key mismatch between messages returns error",
			message:       multipleMsg,
			injectionPath: []string{"b", "ab"},
			injectionValues: map[string]string{
				"test_key": "value",
			},
			wantError: cmpopts.AnyError,
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

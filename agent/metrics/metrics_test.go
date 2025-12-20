/*
Copyright 2021 Google Inc. All rights reserved.
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

package metrics

import (
	"context"
	"expvar"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	gax "github.com/googleapis/gax-go/v2"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
)

// fakeMetricClient is a stub of MetricClient for the purpose of testing
type fakeMetricClient struct {
	Requests []*monitoringpb.CreateTimeSeriesRequest
}

func (f *fakeMetricClient) CreateTimeSeries(ctx context.Context, req *monitoringpb.CreateTimeSeriesRequest, opts ...gax.CallOption) error {
	f.Requests = append(f.Requests, req)
	return nil
}

// NewFakeMetricHandler instantiates a fake metric client for the purpose of testing
func NewFakeMetricHandler(ctx context.Context, projectID, resourceType, resourceKeyValues, metricDomain string) (*MetricHandler, error) {
	client := fakeMetricClient{}
	return newMetricHandlerHelper(ctx, projectID, resourceType, resourceKeyValues, metricDomain, &client)
}

func TestNewMetricHandler(t *testing.T) {
	c := context.Background()
	testCases := []struct {
		projectID         string
		resourceType      string
		resourceKeyValues string
		metricDomain      string
		want              *MetricHandler
	}{
		{
			projectID:         "fake-project",
			resourceType:      "gce_instance",
			resourceKeyValues: "instance-id=fake-instance,instance-zone=fake-zone",
			metricDomain:      "fake-domain.googleapis.com",
			want: &MetricHandler{
				projectID:    "fake-project",
				metricDomain: "fake-domain.googleapis.com",
				resourceType: "gce_instance",
				resourceLabels: &map[string]string{
					"instance_id": "fake-instance",
					"zone":        "fake-zone",
				},
				ctx:    c,
				client: &fakeMetricClient{},
			},
		},
	}

	for _, tc := range testCases {
		got, err := NewFakeMetricHandler(c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain)
		want := tc.want
		if err != nil {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got unexpected error: %v", c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain, err)
		}
		if got.projectID != want.projectID {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got projectID=%v, want projectID=%v", c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain, got.projectID, want.projectID)
		}
		if got.metricDomain != want.metricDomain {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got metricDomain=%v, want metricDomain=%v", c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain, got.metricDomain, want.metricDomain)
		}
		if got.resourceType != want.resourceType {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got resourceType=%v, want resourceType=%v", c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain, got.resourceType, want.resourceType)
		}
		if !reflect.DeepEqual(got.resourceLabels, want.resourceLabels) {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got resourceLabels=%v, want resourceLabels=%v", c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain, got.resourceLabels, want.resourceLabels)
		}
	}
}

func TestNewMetricHandler_MissingArgs(t *testing.T) {
	c := context.Background()
	testCases := []struct {
		projectID         string
		resourceType      string
		resourceKeyValues string
		metricDomain      string
		wantErr           bool
	}{
		{
			projectID:         "",
			metricDomain:      "",
			resourceType:      "",
			resourceKeyValues: "",
			wantErr:           true,
		},
		{
			projectID:         "fake-project",
			metricDomain:      "",
			resourceType:      "",
			resourceKeyValues: "",
			wantErr:           true,
		},
		{
			projectID:         "fake-project",
			metricDomain:      "fake-domain.googleapis.com",
			resourceType:      "",
			resourceKeyValues: "",
			wantErr:           true,
		},
		{
			projectID:         "fake-project",
			metricDomain:      "fake-domain.googleapis.com",
			resourceType:      "gce_instance",
			resourceKeyValues: "",
			wantErr:           true,
		},
		{
			projectID:         "fake-project",
			metricDomain:      "fake-domain.googleapis.com",
			resourceType:      "gce_instance",
			resourceKeyValues: "instance-id=fake-instance",
			wantErr:           true,
		},
	}

	for _, tc := range testCases {
		_, err := NewFakeMetricHandler(c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain)
		got := err != nil
		want := tc.wantErr
		if got != want {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): expected an error, but got nil", c, tc.projectID, tc.resourceType, tc.resourceKeyValues, tc.metricDomain)
		}
	}
}

func TestParseResourceLabels(t *testing.T) {
	testCases := []struct {
		resourceKeyValues string
		labels            map[string]string
		want              map[string]string
		wantErr           bool
	}{
		{
			resourceKeyValues: "",
			labels: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			want: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			wantErr: false,
		},
		{
			resourceKeyValues: "instance-id=fake-instance",
			labels: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			want: map[string]string{
				"instance-id":   "fake-instance",
				"instance-zone": "",
			},
			wantErr: false,
		},
		{
			resourceKeyValues: "instance-zone=fake-zone",
			labels: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			want: map[string]string{
				"instance-id":   "",
				"instance-zone": "fake-zone",
			},
			wantErr: false,
		},
		{
			resourceKeyValues: "instance-id=fake-instance,instance-zone=fake-zone",
			labels: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			want: map[string]string{
				"instance-id":   "fake-instance",
				"instance-zone": "fake-zone",
			},
			wantErr: false,
		},
		{
			resourceKeyValues: "instance-zone==fake-zone",
			labels: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			want: map[string]string{
				"instance-id":   "",
				"instance-zone": "",
			},
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		err := parseResourceLabels(tc.resourceKeyValues, &tc.labels)
		got := tc.labels
		hasErr := err != nil
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseResourceLabels(%v): got resourceLabels=%v, want resourceLabels=%v", tc.resourceKeyValues, got, tc.want)
		}
		if hasErr != tc.wantErr {
			t.Errorf("parseResourceLabels(%v): expected an error, but got nil", tc.resourceKeyValues)
		}
	}
}

func TestGetResponseCountMetricType_Empty(t *testing.T) {
	var metricHandler *MetricHandler
	if res := metricHandler.GetResponseCountMetricType(); res != "" {
		t.Errorf("GetResponseCountMetricType(): got: %v, want: ''", res)
	}
}

func TestWriteResponseCodeMetric_Empty(t *testing.T) {
	var metricHandler *MetricHandler
	if res := metricHandler.WriteResponseCodeMetric(200); res != nil {
		t.Errorf("WriteResponseCodeMetric(): got: %v, want: %v", res, nil)
	}
}

func TestRecordResponseCode(t *testing.T) {
	// Reset expvar map for clean test
	responseCodes = expvar.NewMap("response_codes_test")

	testCases := []struct {
		statusCode int
		count      int
		want       string
	}{
		{200, 1, "1"},
		{200, 2, "3"},
		{404, 1, "1"},
		{500, 1, "1"},
	}

	for _, tc := range testCases {
		for i := 0; i < tc.count; i++ {
			RecordResponseCode(tc.statusCode)
		}
		codeStr := fmt.Sprintf("%d", tc.statusCode)
		got := responseCodes.Get(codeStr)
		if got == nil {
			t.Errorf("RecordResponseCode(%d): code not recorded in expvar", tc.statusCode)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("RecordResponseCode(%d) called %d times: got %v, want %v", tc.statusCode, tc.count, got.String(), tc.want)
		}
	}
}

func TestWriteResponseCodeMetric(t *testing.T) {
	c := context.Background()
	h, err := NewFakeMetricHandler(c, "test-project", "gce_instance", "instance-id=test-id,instance-zone=test-zone", "test-domain.googleapis.com")
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	testCases := []struct {
		statusCode int
		callCount  int
	}{
		{200, 3},
		{404, 1},
		{500, 2},
	}

	for _, tc := range testCases {
		for i := 0; i < tc.callCount; i++ {
			if err := h.WriteResponseCodeMetric(tc.statusCode); err != nil {
				t.Errorf("WriteResponseCodeMetric(%d): unexpected error: %v", tc.statusCode, err)
			}
		}
	}

	// Verify counts accumulated correctly
	h.mu.Lock()
	defer h.mu.Unlock()

	if codeCount["200"] != 3 {
		t.Errorf("WriteResponseCodeMetric(200) called 3 times: got count %d, want 3", codeCount["200"])
	}
	if codeCount["404"] != 1 {
		t.Errorf("WriteResponseCodeMetric(404) called 1 time: got count %d, want 1", codeCount["404"])
	}
	if codeCount["500"] != 2 {
		t.Errorf("WriteResponseCodeMetric(500) called 2 times: got count %d, want 2", codeCount["500"])
	}
}

func TestEmitResponseCodeMetric(t *testing.T) {
	c := context.Background()
	client := &fakeMetricClient{}
	h, err := newMetricHandlerHelper(c, "test-project", "gce_instance", "instance-id=test-id,instance-zone=test-zone", "test-domain.googleapis.com", client)
	if err != nil {
		t.Fatalf("Failed to create handler: %v", err)
	}

	// Record some response codes
	h.WriteResponseCodeMetric(200)
	h.WriteResponseCodeMetric(200)
	h.WriteResponseCodeMetric(404)
	h.WriteResponseCodeMetric(500)

	// Emit metrics using emitMetrics (which also resets)
	h.emitMetrics()

	// Verify requests sent to fake client
	if len(client.Requests) != 3 {
		t.Errorf("emitMetrics(): got %d requests, want 3", len(client.Requests))
	}

	// Verify metric type and labels
	for _, req := range client.Requests {
		if len(req.TimeSeries) != 1 {
			t.Errorf("Request has %d time series, want 1", len(req.TimeSeries))
			continue
		}

		ts := req.TimeSeries[0]
		if ts.Metric.Type != "test-domain.googleapis.com/instance/proxy_agent/response_count" {
			t.Errorf("Wrong metric type: got %s", ts.Metric.Type)
		}

		// Verify labels exist
		if _, ok := ts.Metric.Labels["response_code"]; !ok {
			t.Error("Missing response_code label")
		}
		if _, ok := ts.Metric.Labels["response_code_class"]; !ok {
			t.Error("Missing response_code_class label")
		}
	}

	// Verify counts are reset after emission
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(codeCount) != 0 {
		t.Errorf("codeCount not reset after emission: got %d entries, want 0", len(codeCount))
	}
}

func TestCalculatePercentile(t *testing.T) {
	testCases := []struct {
		name       string
		percentile float64
		durations  []time.Duration
		want       float64
	}{
		{
			name:       "empty",
			percentile: 50.0,
			durations:  []time.Duration{},
			want:       0.0,
		},
		{
			name:       "single value",
			percentile: 50.0,
			durations:  []time.Duration{100 * time.Millisecond},
			want:       100.0,
		},
		{
			name:       "p50",
			percentile: 50.0,
			durations:  []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
			want:       20.0,
		},
		{
			name:       "p99",
			percentile: 99.0,
			durations:  []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
			want:       29.8,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := calculatePercentile(tc.percentile, tc.durations)
			if math.Abs(got-tc.want) > 0.01 {
				t.Errorf("calculatePercentile(%v, %v): got %v, want %v", tc.percentile, tc.durations, got, tc.want)
			}
		})
	}
}

func TestRecordResponseTime(t *testing.T) {
	// Reset latencies for clean test
	latenciesMutex.Lock()
	latencies = make([]time.Duration, 0)
	latenciesMutex.Unlock()

	testLatencies := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	}

	for _, lat := range testLatencies {
		RecordResponseTime(lat)
	}

	latenciesMutex.Lock()
	defer latenciesMutex.Unlock()

	if len(latencies) != len(testLatencies) {
		t.Errorf("RecordResponseTime(): got %d latencies, want %d", len(latencies), len(testLatencies))
	}

	for i, want := range testLatencies {
		if latencies[i] != want {
			t.Errorf("RecordResponseTime(): latencies[%d] = %v, want %v", i, latencies[i], want)
		}
	}
}

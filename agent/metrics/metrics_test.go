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
	"fmt"
	"testing"

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
func NewFakeMetricHandler(ctx context.Context, projectID, monitoringKeyValues, metricDomain string) (*MetricHandler, error) {
	client := fakeMetricClient{}
	return newMetricHandlerHelper(ctx, projectID, monitoringKeyValues, metricDomain, &client)
}

func TestNewMetricHandler(t *testing.T) {
	c := context.Background()
	testCases := []struct {
		projectID           string
		monitoringKeyValues string
		metricDomain        string
		want                *MetricHandler
	}{
		{
			projectID:           "fake-project",
			monitoringKeyValues: "instance-id=fake-instance,instance-zone=fake-zone",
			metricDomain:        "fake-domain.googleapis.com",
			want: &MetricHandler{
				projectID:    "fake-project",
				instanceID:   "fake-instance",
				instanceZone: "fake-zone",
				metricDomain: "fake-domain.googleapis.com",
				ctx:          c,
				client:       &fakeMetricClient{},
			},
		},
	}

	for _, tc := range testCases {
		got, err := NewFakeMetricHandler(c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain)
		want := tc.want
		if err != nil {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v): got unexpected error: %v", c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain, err)
		}
		if got.projectID != want.projectID {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v): wrong projectID: got: %v, want: %v", c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain, got.projectID, want.projectID)
		}
		if got.instanceID != want.instanceID {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v): wrong instanceID: got: %v, want: %v", c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain, got.instanceID, want.instanceID)
		}
		if got.instanceZone != want.instanceZone {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v): wrong instanceZone: got: %v, want: %v", c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain, got.instanceZone, want.instanceZone)
		}
		if got.metricDomain != want.metricDomain {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v): wrong metricDomain: got: %v, want: %v", c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain, got.metricDomain, want.metricDomain)
		}
	}
}

func TestNewMetricHandler_MissingArgs(t *testing.T) {
	c := context.Background()
	testCases := []struct {
		projectID           string
		monitoringKeyValues string
		metricDomain        string
		err                 error
	}{
		{
			projectID:           "",
			monitoringKeyValues: "",
			metricDomain:        "",
			err:                 fmt.Errorf("Failed to create metric handler: missing projectID"),
		},
		{
			projectID:           "fake-project",
			monitoringKeyValues: "",
			metricDomain:        "",
			err:                 fmt.Errorf("Failed to create metric handler: missing instanceID"),
		},
		{
			projectID:           "fake-project",
			monitoringKeyValues: "instance-id=fake-instance,instance-zone=",
			metricDomain:        "",
			err:                 fmt.Errorf("Failed to create metric handler: missing instanceZone"),
		},
		{
			projectID:           "fake-project",
			monitoringKeyValues: "instance-id=fake-instance,instance-zone=fake-zone",
			metricDomain:        "",
			err:                 fmt.Errorf("Failed to create metric handler: missing metricDomain"),
		},
	}

	for _, tc := range testCases {
		_, got := NewFakeMetricHandler(c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain)
		want := tc.err
		if got == nil || got.Error() != want.Error() {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v): got: %v, want: %v", c, tc.projectID, tc.monitoringKeyValues, tc.metricDomain, got, want)
		}
	}
}

func TestParseMonitoringKeyValues(t *testing.T) {
	testCases := []struct {
		monitoringKeyValues string
		wantedInstanceID    string
		wantedInstanceZone  string
		err                 error
	}{
		{
			monitoringKeyValues: "",
			wantedInstanceID:    "",
			wantedInstanceZone:  "",
			err:                 nil,
		},
		{
			monitoringKeyValues: "instance-id=fake-instance",
			wantedInstanceID:    "fake-instance",
			wantedInstanceZone:  "",
			err:                 nil,
		},
		{
			monitoringKeyValues: "instance-zone=fake-zone",
			wantedInstanceID:    "",
			wantedInstanceZone:  "fake-zone",
			err:                 nil,
		},
		{
			monitoringKeyValues: "instance-id=fake-instance,instance-zone=fake-zone",
			wantedInstanceID:    "fake-instance",
			wantedInstanceZone:  "fake-zone",
			err:                 nil,
		},
		{
			monitoringKeyValues: "instance-id=fake-instance,instance-zone==fake-zone",
			wantedInstanceID:    "",
			wantedInstanceZone:  "",
			err:                 fmt.Errorf("Error parsing monitoringKeyValue('instance-zone==fake-zone'): got 3 expressions, wanted 2"),
		},
	}

	for _, tc := range testCases {
		gotInstanceID, gotInstanceZone, err := parseMonitoringKeyValues(tc.monitoringKeyValues)
		if gotInstanceID != tc.wantedInstanceID {
			t.Errorf("parseMonitoringKeyValues(%v): got instanceID=%v, want instanceID=%v", tc.monitoringKeyValues, gotInstanceID, tc.wantedInstanceID)
		}
		if gotInstanceZone != tc.wantedInstanceZone {
			t.Errorf("parseMonitoringKeyValues(%v): got instanceZone=%v, want instanceZone=%v", tc.monitoringKeyValues, gotInstanceZone, tc.wantedInstanceZone)
		}
		if err == nil && tc.err != nil || err != nil && tc.err == nil {
			t.Errorf("parseMonitoringKeyValues(%v): got err='%v', want err='%v'", tc.monitoringKeyValues, err, tc.err)
		}
	}
}

func TestGetResponseCountMetricType_Empty(t *testing.T) {
	var metricHandler *MetricHandler
	if res := metricHandler.GetResponseCountMetricType(); res != "" {
		t.Errorf("GetResponseCountMetricType(): got: %v, want: ''", res)
	}
}

func TestWriteMetric_Empty(t *testing.T) {
	var metricHandler *MetricHandler
	if res := metricHandler.WriteMetric(metricHandler.GetResponseCountMetricType(), 200); res != nil {
		t.Errorf("WriteMetric(): got: %v, want: %v", res, nil)
	}
}

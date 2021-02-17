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
	"reflect"
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

func TestWriteMetric_Empty(t *testing.T) {
	var metricHandler *MetricHandler
	if res := metricHandler.WriteMetric(metricHandler.GetResponseCountMetricType(), 200); res != nil {
		t.Errorf("WriteMetric(): got: %v, want: %v", res, nil)
	}
}

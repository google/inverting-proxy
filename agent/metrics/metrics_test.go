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
)

func TestNewMetricHandler(t *testing.T) {
	c := context.Background()
	testCases := []struct {
		projectID    string
		instanceID   string
		instanceZone string
		metricDomain string
		want         *MetricHandler
	}{
		{
			projectID:    "fake-project",
			instanceID:   "fake-instance",
			instanceZone: "fake-zone",
			metricDomain: "fake-domain.googleapis.com",
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
		got, err := NewFakeMetricHandler(c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain)
		want := tc.want
		if err != nil {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got unexpected error: %v", c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain, err)
		}
		if got.projectID != want.projectID {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): wrong projectID: got: %v, want: %v", c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain, got.projectID, want.projectID)
		}
		if got.instanceID != want.instanceID {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): wrong instanceID: got: %v, want: %v", c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain, got.instanceID, want.instanceID)
		}
		if got.instanceZone != want.instanceZone {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): wrong instanceZone: got: %v, want: %v", c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain, got.instanceZone, want.instanceZone)
		}
		if got.metricDomain != want.metricDomain {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): wrong metricDomain: got: %v, want: %v", c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain, got.metricDomain, want.metricDomain)
		}
	}
}

func TestNewMetricHandler_MissingArgs(t *testing.T) {
	c := context.Background()
	testCases := []struct {
		projectID    string
		instanceID   string
		instanceZone string
		metricDomain string
		err          error
	}{
		{
			projectID:    "",
			instanceID:   "",
			instanceZone: "",
			metricDomain: "",
			err:          fmt.Errorf("Failed to create metric handler: missing projectID"),
		},
		{
			projectID:    "fake-project",
			instanceID:   "",
			instanceZone: "",
			metricDomain: "",
			err:          fmt.Errorf("Failed to create metric handler: missing instanceID"),
		},
		{
			projectID:    "fake-project",
			instanceID:   "fake-instance",
			instanceZone: "",
			metricDomain: "",
			err:          fmt.Errorf("Failed to create metric handler: missing instanceZone"),
		},
		{
			projectID:    "fake-project",
			instanceID:   "fake-instance",
			instanceZone: "fake-zone",
			metricDomain: "",
			err:          fmt.Errorf("Failed to create metric handler: missing metricDomain"),
		},
	}

	for _, tc := range testCases {
		_, got := NewFakeMetricHandler(c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain)
		want := tc.err
		if got == nil || got.Error() != want.Error() {
			t.Errorf("NewFakeMetricHandler(%v, %v, %v, %v, %v): got: %v, want: %v", c, tc.projectID, tc.instanceID, tc.instanceZone, tc.metricDomain, got, want)
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

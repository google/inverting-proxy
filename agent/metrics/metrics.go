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
	"log"
	"strings"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3"
	googlepb "github.com/golang/protobuf/ptypes/timestamp"
	gax "github.com/googleapis/gax-go/v2"
	apioption "google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
)

const (
	samplePeriod = 60 * time.Second
)

var (
	responseCountResourceKeyToFlagName = map[string]string{
		"instance_id": "instance-id",
		"zone":        "instance-zone",
	}
)

var startTime time.Time
var codeCount map[string]int64

// metricClient is a client for interacting with Cloud Monitoring API.
type metricClient interface {
	CreateTimeSeries(ctx context.Context, req *monitoringpb.CreateTimeSeriesRequest, opts ...gax.CallOption) error
}

type MetricHandler struct {
	projectID      string
	metricDomain   string
	resourceType   string
	resourceLabels *map[string]string
	ctx            context.Context
	client         metricClient
}

// NewMetricHandler instantiates a metric client for the purpose of writing metrics to cloud monarch
func NewMetricHandler(ctx context.Context, projectID, resourceType, resourceKeyValues, metricDomain, endpoint string) (*MetricHandler, error) {
	log.Printf("NewMetricHandler|instantiating metric handler")
	client, err := monitoring.NewMetricClient(
		ctx,
		apioption.WithEndpoint(endpoint),
	)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
		return nil, err
	}
	return newMetricHandlerHelper(ctx, projectID, resourceType, resourceKeyValues, metricDomain, client)
}

func newMetricHandlerHelper(ctx context.Context, projectID, resourceType, resourceKeyValues, metricDomain string, client metricClient) (*MetricHandler, error) {
	if projectID == "" {
		err := fmt.Errorf("Failed to create metric handler: missing projectID")
		return nil, err
	}
	if metricDomain == "" {
		err := fmt.Errorf("Failed to create metric handler: missing metricDomain")
		return nil, err
	}

	var resourceLabels *map[string]string
	if resourceType == "" {
		err := fmt.Errorf("Failed to create metric handler: missing resourceType")
		return nil, err
	}
	if resourceType == "gce_instance" {
		res, err := parseGCEResourceLabels(resourceKeyValues)
		if err != nil {
			return nil, err
		}
		resourceLabels = res
	} else {
		err := fmt.Errorf("Failed to create metric handler: unknown resource type %v", resourceType)
		return nil, err
	}

	startTime = time.Now()
	codeCount = make(map[string]int64)

	return &MetricHandler{
		projectID:      projectID,
		metricDomain:   metricDomain,
		resourceType:   resourceType,
		resourceLabels: resourceLabels,
		ctx:            ctx,
		client:         client,
	}, nil
}

func parseGCEResourceLabels(resourceKeyValues string) (*map[string]string, error) {
	flags := map[string]string{
		"instance-id":   "",
		"instance-zone": "",
	}
	res := map[string]string{
		"instance_id": "",
		"zone":        "",
	}
	err := parseResourceLabels(resourceKeyValues, &flags)
	for key, _ := range res {
		flagKey := responseCountResourceKeyToFlagName[key]
		flagValue := flags[flagKey]
		if flagValue == "" {
			err := fmt.Errorf("Failed to create metric handler: missing gce_instance resource label (%v)", key)
			return &res, err
		}
		res[key] = flagValue
	}
	return &res, err
}

func parseResourceLabels(resourceKeyValues string, labels *map[string]string) error {
	if resourceKeyValues == "" {
		return nil
	}
	pairs := strings.Split(resourceKeyValues, ",")
	for _, p := range pairs {
		pair := strings.Split(p, "=")
		if len(pair) != 2 {
			err := fmt.Errorf("Error parsing monitoringKeyValue('%v'): got %v expressions, wanted 2", p, len(pair))
			return err
		}
		key, value := pair[0], pair[1]
		if _, ok := (*labels)[key]; ok {
			(*labels)[key] = value
		}
	}
	return nil
}

func (h *MetricHandler) WriteMetric(metricType string, statusCode int) error {
	if h == nil {
		return nil
	}
	if metricType == h.GetResponseCountMetricType() {
		err := h.writeResponseCodeMetric(statusCode)
		return err
	}
	return nil
}

func (h *MetricHandler) GetResponseCountMetricType() string {
	if h == nil {
		return ""
	}
	return fmt.Sprintf("%s/instance/proxy_agent/response_count", h.metricDomain)
}

// writeResponseCodeMetric will gather response codes and write to cloud monarch once sample period is over
func (h *MetricHandler) writeResponseCodeMetric(statusCode int) error {
	responseCode := fmt.Sprintf("%v", statusCode)

	// Update response code count for the current sample period
	if _, ok := codeCount[responseCode]; ok {
		codeCount[responseCode] += 1
	} else {
		codeCount[responseCode] = 1
	}

	// Only write time series after sample period is over
	if time.Since(startTime) < samplePeriod {
		return nil
	}

	// Write metric
	log.Printf("WriteMetric|attempting to write metrics at time: %v\n", time.Now())
	for responseCode, count := range codeCount {
		responseClass := fmt.Sprintf("%sXX", responseCode[0:1])
		metricLabels := map[string]string{
			"response_code":       responseCode,
			"response_code_class": responseClass,
		}
		timeSeries := h.newTimeSeries(h.GetResponseCountMetricType(), metricLabels, newDataPoint(count))

		log.Printf("got %v occurances of %s response code\n", count, responseCode)
		log.Printf("%v\n\n", timeSeries)

		if err := h.client.CreateTimeSeries(h.ctx, &monitoringpb.CreateTimeSeriesRequest{
			Name:       monitoring.MetricProjectPath(h.projectID),
			TimeSeries: []*monitoringpb.TimeSeries{timeSeries},
		}); err != nil {
			log.Println("Failed to write time series data: ", err)
			return err
		}
	}

	// Clean up
	startTime = time.Now()
	codeCount = make(map[string]int64)

	return nil
}

// newTimeSeries creates and returns a new time series
func (h *MetricHandler) newTimeSeries(metricType string, metricLabels map[string]string, dataPoint *monitoringpb.Point) *monitoringpb.TimeSeries {
	return &monitoringpb.TimeSeries{
		Metric: &metricpb.Metric{
			Type:   metricType,
			Labels: metricLabels,
		},
		Resource: &monitoredrespb.MonitoredResource{
			Type:   h.resourceType,
			Labels: *h.resourceLabels,
		},
		Points: []*monitoringpb.Point{
			dataPoint,
		},
	}
}

// newDataPoint creates and returns a new data point
func newDataPoint(count int64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{
			StartTime: &googlepb.Timestamp{
				Seconds: time.Now().Unix(),
			},
			EndTime: &googlepb.Timestamp{
				Seconds: time.Now().Unix() + 1,
			},
		},
		Value: &monitoringpb.TypedValue{
			Value: &monitoringpb.TypedValue_Int64Value{
				Int64Value: count,
			},
		},
	}
}

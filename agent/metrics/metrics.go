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
	"sync"
	"time"

	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
	googlepb "github.com/golang/protobuf/ptypes/timestamp"
	monitoring "cloud.google.com/go/monitoring/apiv3"
	gax "github.com/googleapis/gax-go/v2"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
	apioption "google.golang.org/api/option"
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

var codeCount map[string]int64

// metricClient is a client for interacting with Cloud Monitoring API.
type metricClient interface {
	CreateTimeSeries(ctx context.Context, req *monitoringpb.CreateTimeSeriesRequest, opts ...gax.CallOption) error
}

// MetricHandler handles metrics collections/writes to cloud monarch
type MetricHandler struct {
	mu             sync.Mutex
	projectID      string
	metricDomain   string
	resourceType   string
	resourceLabels *map[string]string
	ctx            context.Context
	client         metricClient
}

// NewMetricHandler instantiates a metric client for the purpose of writing metrics to cloud monarch
func NewMetricHandler(ctx context.Context, projectID, resourceType, resourceKeyValues, metricDomain, endpoint string) (*MetricHandler, error) {
	if projectID == "" || resourceType == "" || resourceKeyValues == "" || metricDomain == "" || endpoint == "" {
		log.Printf("Skipping metric handler initialization due to empty arguments.")
		return nil, nil
	}
	log.Printf("NewMetricHandler|instantiating metric handler")
	client, err := monitoring.NewMetricClient(
		ctx,
		apioption.WithEndpoint(endpoint),
	)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
		return nil, err
	}

	handler, err := newMetricHandlerHelper(ctx, projectID, resourceType, resourceKeyValues, metricDomain, client)
	if err != nil {
		return nil, err
	}

	go func() {
		log.Printf("NewMetricHandler|success metric handler ready")
		ticker := time.NewTicker(samplePeriod)
		defer ticker.Stop()
		for {
			select {
			case <-handler.ctx.Done():
				return
			case <-ticker.C:
				handler.emitResponseCodeMetric()
				handler.mu.Lock()
				codeCount = make(map[string]int64)
				handler.mu.Unlock()
			}
		}
	}()

	return handler, nil
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
	for key := range res {
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

// GetResponseCountMetricType constructs and returns a string representing the response_count metric type
func (h *MetricHandler) GetResponseCountMetricType() string {
	if h == nil {
		return ""
	}
	return fmt.Sprintf("%s/instance/proxy_agent/response_count", h.metricDomain)
}

// WriteResponseCodeMetric will record observed response codes and emitResponseCodeMetric writes to cloud monarch
func (h *MetricHandler) WriteResponseCodeMetric(statusCode int) error {
	if h == nil {
		return nil
	}
	responseCode := fmt.Sprintf("%v", statusCode)

	// Update response code count for the current sample period
	h.mu.Lock()
	codeCount[responseCode]++
	h.mu.Unlock()

	return nil
}

// emitResponseCodeMetric emits observed response codes to cloud monarch once sample period is over
func (h *MetricHandler) emitResponseCodeMetric() {
	log.Printf("WriteResponseCodeMetric|attempting to write metrics at time: %v\n", time.Now())
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
			Name:       fmt.Sprintf("projects/%s", h.projectID),
			TimeSeries: []*monitoringpb.TimeSeries{timeSeries},
		}); err != nil {
			log.Println("Failed to write time series data: ", err)
		}
	}
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

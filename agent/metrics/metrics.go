/*
Copyright 2019 Google Inc. All rights reserved.
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
        "time"

	apioption "google.golang.org/api/option"
        googlepb "github.com/golang/protobuf/ptypes/timestamp"
        metricpb "google.golang.org/genproto/googleapis/api/metric"
        monitoring "cloud.google.com/go/monitoring/apiv3"
        monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
        monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
)

const (
	samplePeriod	= 60 * time.Second
        endpoint	= "staging-monitoring.sandbox.googleapis.com:443"
)

var startTime time.Time
var codeCount map[string]int64

type MetricHandler struct {
	projectID string
	instanceID string
	instanceZone string
	metricDomain string
	ctx context.Context
	client *monitoring.MetricClient
}

// NewMetricHandler instantiates a metric client for the purpose of writing metrics to cloud monarch
func NewMetricHandler(ctx context.Context, projectID, instanceID, instanceZone, metricDomain string) (*MetricHandler, error) {
	log.Printf("NewMetricHandler|instantiating metric handler")
	client, err := monitoring.NewMetricClient(
                ctx,
                apioption.WithEndpoint(endpoint),
        )
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
		return nil, err
	}
	startTime = time.Now()
	codeCount = make(map[string]int64)

	return &MetricHandler{
		projectID:	projectID,
		instanceID:	instanceID,
		instanceZone:	instanceZone,
		metricDomain:	metricDomain,
		ctx:		ctx,
		client:		client,
	}, nil
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
		timeSeries := h.newTimeSeries(h.GetResponseCountMetricType(), responseCode, responseClass, newDataPoint(count))

		log.Printf("got %v occurances of %s response code\n", count, responseCode)
		log.Printf("%v\n\n", timeSeries)

		if err := h.client.CreateTimeSeries(h.ctx, &monitoringpb.CreateTimeSeriesRequest{
			Name: monitoring.MetricProjectPath(h.projectID),
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
func (h *MetricHandler) newTimeSeries(metricType, responseCode, responseClass string, dataPoint *monitoringpb.Point) *monitoringpb.TimeSeries {
        return &monitoringpb.TimeSeries{
		Metric: &metricpb.Metric{
			Type: metricType,
			Labels: map[string]string{
				"response_code": responseCode,
				"response_code_class": responseClass,
			},
		},
		Resource: &monitoredrespb.MonitoredResource{
			Type: "gce_instance",
			Labels: map[string]string{
				"instance_id": h.instanceID,
				"zone": h.instanceZone,
			},
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
                                Seconds: time.Now().Unix()+1,
                        },
                },
                Value: &monitoringpb.TypedValue{
                        Value: &monitoringpb.TypedValue_Int64Value{
                                Int64Value: count,
                        },
                },
        }
}

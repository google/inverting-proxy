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
        endpoint	= "staging-monitoring.sandbox.googleapis.com:443"
        ResponseCount	= "notebooks.googleapis.com/instance/proxy_agent/response_count"
        CrashCount	= "notebooks.googleapis.com/instance/proxy_agent/crash_count"
)

type MetricHandler struct {
	projectID string
	instanceID string
	instanceZone string
	ctx context.Context
	client *monitoring.MetricClient
}

func NewMetricHandler(ctx context.Context, projectID, instanceID, instanceZone string) (*MetricHandler, error) {
	client, err := monitoring.NewMetricClient(
                ctx,
                apioption.WithEndpoint(endpoint),
        )
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
		return nil, err
	}

	return &MetricHandler{
		projectID:	"pekopeko-test",
		instanceID:	"fake-instance",
		instanceZone:	"us-west1-a",
		ctx:		ctx,
		client:		client,
	}, nil
}

func (h *MetricHandler) WriteMetric(metricType string, statusCode int) error {
	responseCode := fmt.Sprintf("%v", statusCode)
	responseClass := fmt.Sprintf("%sXX", responseCode[0:1])
        dataPoint := newDataPoint()
        if err := h.client.CreateTimeSeries(h.ctx, &monitoringpb.CreateTimeSeriesRequest{
                Name: monitoring.MetricProjectPath(h.projectID),
                TimeSeries: []*monitoringpb.TimeSeries{
                        {
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
                        },
                },
        }); err != nil {
                log.Fatalf("Failed to write time series data: %v", err)
                return err
        }
	return nil
}

func newDataPoint() *monitoringpb.Point {
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
                                Int64Value: 1,
                        },
                },
        }
}


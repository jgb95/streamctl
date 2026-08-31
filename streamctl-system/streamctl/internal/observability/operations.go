package observability

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"streamctl/internal/db"
	"streamctl/internal/systemd"
)

type operationsCollector struct {
	db      *db.DB
	systemd *systemd.Manager

	streams           *prometheus.Desc
	streamActive      *prometheus.Desc
	streamFailed      *prometheus.Desc
	streamIngress     *prometheus.Desc
	streamEgress      *prometheus.Desc
	destinationEgress *prometheus.Desc
	nextTrigger       *prometheus.Desc
	streamEndpoints   *prometheus.Desc
	gpuJobs           *prometheus.Desc
	gpuAttempts       *prometheus.Desc
	gpuOldest         *prometheus.Desc
}

func NewOperationsCollector(database *db.DB, manager *systemd.Manager) prometheus.Collector {
	return &operationsCollector{
		db: database, systemd: manager,
		streams:           prometheus.NewDesc("streamctl_streams", "Streams by operational state.", []string{"state"}, nil),
		streamActive:      prometheus.NewDesc("streamctl_stream_active", "Whether a configured stream service is currently active.", []string{"stream", "stream_id"}, nil),
		streamFailed:      prometheus.NewDesc("streamctl_stream_failed", "Whether the latest stream service activation failed.", []string{"stream", "stream_id"}, nil),
		streamIngress:     prometheus.NewDesc("streamctl_stream_ingress_bytes_total", "Bytes received by the current or latest stream service activation.", []string{"stream", "stream_id"}, nil),
		streamEgress:      prometheus.NewDesc("streamctl_stream_egress_bytes_total", "Bytes sent by the current or latest stream service activation.", []string{"stream", "stream_id"}, nil),
		destinationEgress: prometheus.NewDesc("streamctl_stream_destination_egress_bytes_estimate_total", "Estimated bytes sent to an enabled remote destination during the current or latest stream activation. The service cgroup total is divided evenly across enabled tee outputs.", []string{"stream", "stream_id", "destination", "endpoint_type"}, nil),
		nextTrigger:       prometheus.NewDesc("streamctl_stream_next_trigger_timestamp_seconds", "Next scheduled stream activation as a Unix timestamp.", []string{"stream", "stream_id"}, nil),
		streamEndpoints:   prometheus.NewDesc("streamctl_stream_endpoints", "Enabled destination assignments across configured streams.", []string{"type"}, nil),
		gpuJobs:           prometheus.NewDesc("streamctl_gpu_jobs", "GPU queue entries by state.", []string{"state"}, nil),
		gpuAttempts:       prometheus.NewDesc("streamctl_gpu_job_attempts_total", "Total GPU processing attempts recorded by the queue.", nil, nil),
		gpuOldest:         prometheus.NewDesc("streamctl_gpu_queue_oldest_seconds", "Age in seconds of the oldest queued GPU job, or zero when the queue is empty.", nil, nil),
	}
}

func (c *operationsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		c.streams, c.streamActive, c.streamFailed, c.streamIngress, c.streamEgress, c.destinationEgress,
		c.nextTrigger, c.streamEndpoints, c.gpuJobs, c.gpuAttempts, c.gpuOldest,
	} {
		ch <- desc
	}
}

func (c *operationsCollector) Collect(ch chan<- prometheus.Metric) {
	streams, err := c.db.ListStreams()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.streams, err)
		return
	}

	configured, enabled, active, failed := len(streams), 0, 0, 0
	endpointCounts := map[string]int{}
	for i := range streams {
		stream := &streams[i]
		if stream.Enabled {
			enabled++
		}
		for _, endpoint := range stream.Endpoints {
			if endpoint.Enabled {
				endpointCounts[endpoint.Type]++
			}
		}
		id := strconv.FormatInt(stream.ID, 10)
		runtime, runtimeErr := c.systemd.Runtime(stream.ID)
		if runtimeErr == nil {
			if runtime.Active {
				active++
			}
			if runtime.Failed {
				failed++
			}
			ch <- prometheus.MustNewConstMetric(c.streamActive, prometheus.GaugeValue, boolFloat(runtime.Active), stream.Name, id)
			ch <- prometheus.MustNewConstMetric(c.streamFailed, prometheus.GaugeValue, boolFloat(runtime.Failed), stream.Name, id)
			ch <- prometheus.MustNewConstMetric(c.streamIngress, prometheus.CounterValue, runtime.IngressBytes, stream.Name, id)
			ch <- prometheus.MustNewConstMetric(c.streamEgress, prometheus.CounterValue, runtime.EgressBytes, stream.Name, id)
			destinations := enabledRemoteDestinations(stream.Endpoints)
			if len(destinations) > 0 {
				estimate := runtime.EgressBytes / float64(len(destinations))
				for _, destination := range destinations {
					ch <- prometheus.MustNewConstMetric(c.destinationEgress, prometheus.CounterValue, estimate, stream.Name, id, destination.Name, destination.Type)
				}
			}
		}
		if next := c.systemd.NextTriggerUnix(stream.ID); next > 0 {
			ch <- prometheus.MustNewConstMetric(c.nextTrigger, prometheus.GaugeValue, float64(next), stream.Name, id)
		}
	}
	for state, value := range map[string]int{
		"configured": configured, "enabled": enabled, "active": active, "failed": failed,
	} {
		ch <- prometheus.MustNewConstMetric(c.streams, prometheus.GaugeValue, float64(value), state)
	}
	for endpointType, value := range endpointCounts {
		ch <- prometheus.MustNewConstMetric(c.streamEndpoints, prometheus.GaugeValue, float64(value), endpointType)
	}

	gpu, err := c.db.GPUQueueMetrics()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.gpuJobs, err)
		return
	}
	for _, state := range []string{"queued", "running", "finished", "failed", "cancelled"} {
		ch <- prometheus.MustNewConstMetric(c.gpuJobs, prometheus.GaugeValue, float64(gpu.Counts[state]), state)
	}
	ch <- prometheus.MustNewConstMetric(c.gpuAttempts, prometheus.CounterValue, float64(gpu.Attempts))
	oldest := 0.0
	if gpu.OldestQueuedAt != nil {
		oldest = time.Since(*gpu.OldestQueuedAt).Seconds()
		if oldest < 0 {
			oldest = 0
		}
	}
	ch <- prometheus.MustNewConstMetric(c.gpuOldest, prometheus.GaugeValue, oldest)
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func enabledRemoteDestinations(endpoints []db.Endpoint) []db.Endpoint {
	destinations := make([]db.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.Enabled {
			destinations = append(destinations, endpoint)
		}
	}
	return destinations
}

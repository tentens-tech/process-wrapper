package metrics

import (
	"bufio"
	"encoding/json"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	SidecarCmdDurationSecondsName                = "sidecar_cmd_duration_seconds"
	SidecarCmdReadinessName                      = "sidecar_cmd_readiness_count"
	SidecarCmdCPUSecondsName                     = "sidecar_cmd_cpu_usage_seconds"
	SidecarCmdCPUSecondsGaugeName                = "sidecar_cmd_cpu_usage_seconds_gauge"
	SidecarCmdMemoryUsageName                    = "sidecar_cmd_memory_usage_bytes"
	SidecarCmdHeartBeatPeriodName                = "sidecar_cmd_heartbeat_period_seconds"
	SidecarAcquireLockTotalName                  = "sidecar_cmd_acquire_lock_total"
	SidecarMetricsMergeDurationSecondsBucketName = "sidecar_metrics_merge_duration_seconds"
)

var (
	SidecarCmdReadinessCount                 *prometheus.CounterVec
	SidecarCmdCPUSeconds                     *prometheus.CounterVec
	SidecarAcquireLockTotal                  *prometheus.CounterVec
	SidecarCmdDurationSecondsGauge           *prometheus.GaugeVec
	SidecarCmdMemoryUsageGauge               *prometheus.GaugeVec
	SidecarCmdCPUSecondsGauge                *prometheus.GaugeVec
	SidecarCmdHeartBeatPeriodGauge           *prometheus.GaugeVec
	SidecarMetricsMergeDurationSecondsBucket *prometheus.HistogramVec
)

func PrometheusHandler() {
	http.Handle("/metrics", promhttp.Handler())
}

func init() {
	SidecarCmdReadinessCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: SidecarCmdReadinessName,
		Help: "Sidecar processes readiness status",
	}, []string{"status", "process_name", "cmd"})
	SidecarAcquireLockTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: SidecarAcquireLockTotalName,
		Help: "Sidecar process acquire lock total",
	}, []string{"status", "process_name", "cmd"})
	SidecarCmdDurationSecondsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: SidecarCmdDurationSecondsName,
		Help: "Sidecar processes execution duration",
	}, []string{"process_name", "exit_code"})
	SidecarCmdHeartBeatPeriodGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: SidecarCmdHeartBeatPeriodName,
		Help: "Sidecar processes heartbeat period",
	}, []string{"process_name"})

	SidecarCmdCPUSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: SidecarCmdCPUSecondsName,
		Help: "Sidecar process cpu usage in seconds",
	}, []string{"process_name"})
	SidecarCmdCPUSecondsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: SidecarCmdCPUSecondsGaugeName,
		Help: "Sidecar process cpu usage in seconds gauge",
	}, []string{"process_name"})

	SidecarCmdMemoryUsageGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: SidecarCmdMemoryUsageName,
		Help: "Sidecar process memory usage in bytes",
	}, []string{"process_name"})

	SidecarMetricsMergeDurationSecondsBucket = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    SidecarMetricsMergeDurationSecondsBucketName,
		Buckets: []float64{0.001, 0.005, 0.05, 0.1, 0.5, 1, 5, 10, 20, 50},
		Help:    "Sidecar prommerge duration seconds",
	}, []string{})
	prometheus.MustRegister(
		SidecarAcquireLockTotal,
		SidecarCmdReadinessCount,
		SidecarCmdDurationSecondsGauge,
		SidecarCmdCPUSeconds,
		SidecarCmdCPUSecondsGauge,
		SidecarCmdMemoryUsageGauge,
		SidecarCmdHeartBeatPeriodGauge,
		SidecarMetricsMergeDurationSecondsBucket,
	)
}

var metricsServer *Server
var extraJSONLabels map[string]string
var extraFileLabels map[string]string

type Server struct {
	Connection           net.Conn
	PreemptedMetric      Metric
	TerminationMetric    Metric
	HealthProbeMetric    Metric
	CommandRestartMetric Metric
	sync.RWMutex
}

func NewMetricsServer(address string) *Server {
	if address == "" {
		return nil
	}
	if metricsServer != nil {
		log.Printf("Return metrics-server singleton")
		return metricsServer
	}
	var (
		err error
		c   net.Conn
	)
	c, err = net.Dial("udp", address)
	if err != nil {
		log.Errorf("Cannot connect to metrics-server udp://%v address, %v", address, err)
		return nil
	}

	metricsServer = &Server{
		Connection: c,
	}
	GetExtraJSONLabels()
	GetExtraFileLabels()

	return metricsServer
}

func GetExtraJSONLabels() map[string]string {
	var err error
	if extraJSONLabels != nil {
		return extraJSONLabels
	}
	if extraLabelsJSON, ok := os.LookupEnv("METRICS_JSON_EXTRA_LABELS"); ok {
		extraJSONLabels = make(map[string]string)
		err = json.Unmarshal([]byte(extraLabelsJSON), &extraJSONLabels)
		if err != nil {
			log.Warnf("Cannot load metric json extra labels")
		}
		return extraJSONLabels
	}
	return nil
}

func GetExtraFileLabels() {
	var err error
	if extraFileLabels != nil {
		return
	}

	if extraLabelsFilePath, ok := os.LookupEnv("METRICS_EXTRA_LABELS_FILE_PATH"); ok {
		var extraLabelsFileExcludeList string
		var extraLabelsFileExclude []string
		extraFileLabels = make(map[string]string)
		if extraLabelsFileExcludeList, ok = os.LookupEnv("METRICS_EXTRA_LABELS_FILE_EXCLUDE"); ok {
			extraLabelsFileExclude = append(extraLabelsFileExclude, strings.Split(extraLabelsFileExcludeList, ",")...)
		}

		var labelsFile *os.File
		labelsFile, err = os.Open(extraLabelsFilePath)
		if err != nil {
			log.Errorf("Cannot read the labels file %v", extraLabelsFilePath)
		}
		defer labelsFile.Close()
		reader := bufio.NewReader(labelsFile)
		for {
			var line string
			line, err = reader.ReadString('\n')
			// check if the line has = sign
			// and process the line. Ignore the rest.
			if equal := strings.Index(line, "="); equal >= 0 {
				if key := strings.TrimSpace(line[:equal]); len(key) > 0 {
					value := ""
					if len(line) > equal {
						value = strings.TrimSpace(line[equal+1:])
					}
					// assign the config map
					if !sliceContains(extraLabelsFileExclude, key) {
						// Replace dashes in label names
						key = strings.ReplaceAll(key, "-", "_")
						extraFileLabels[key] = strings.ReplaceAll(value, "\"", "")
					}
				}
			}
			if err == io.EOF {
				break
			}
		}
	}
}

func MergeLabels(merge map[string]string) map[string]string {
	labels := map[string]string{
		"pod":       os.Getenv("HOSTNAME"),
		"namespace": os.Getenv("POD_NAMESPACE"),
		"node":      os.Getenv("NODE_NAME"),
	}

	for k, v := range extraJSONLabels {
		labels[k] = v
	}

	for k, v := range extraFileLabels {
		labels[k] = v
	}

	for k, v := range merge {
		labels[k] = v
	}
	return labels
}

type Metric struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Operation string            `json:"op,omitempty"`
	Help      string            `json:"help,omitempty"`
	Value     float64           `json:"value"`
}

func (s *Server) PushInitPreemptedMetric() {
	if s == nil {
		return
	}
	m := Metric{
		Name:      "preempted",
		Operation: "inc",
		Labels:    MergeLabels(nil),
		Value:     0,
	}
	m.Labels["event_type"] = m.Name
	s.Push(m)
}

func (s *Server) PushPreemptedEvent() {
	if s == nil {
		return
	}
	m := Metric{
		Name:      "preempted",
		Operation: "event",
		Labels:    MergeLabels(nil),
		Value:     1,
	}
	m.Labels["event_type"] = m.Name
	s.Push(m)
	m.Operation = "inc"
	s.Push(m)

	// Count the time before preemption happened
	go func() {
		m.Name = "preemption_count"
		m.Labels["event_type"] = m.Name
		m.Operation = "event"
		for i := 0; i < 60; i++ {
			s.Push(m)
			time.Sleep(time.Second)
		}
	}()
}

func (s *Server) PushTerminationEvent(labels map[string]string) {
	if s == nil {
		return
	}
	m := Metric{
		Name:      "termination",
		Labels:    MergeLabels(labels),
		Operation: "event",
		Value:     1,
	}
	m.Labels["event_type"] = m.Name
	s.Push(m)
}

func (s *Server) PushHealthEvent(labels map[string]string) {
	status, ok := labels["status"]
	if ok && (status == "true" && log.GetLevel() != log.DebugLevel) || s == nil {
		return
	}
	m := Metric{
		Name:      "readiness",
		Labels:    MergeLabels(labels),
		Operation: "event",
		Value:     1,
	}
	m.Labels["event_type"] = m.Name
	s.Push(m)
}

// func (s *Server) PushCommandRestartEvent(command, processName string) {
func (s *Server) PushCommandRestartEvent(labels map[string]string) {
	if s == nil {
		return
	}
	m := Metric{
		Name:      "restart",
		Labels:    MergeLabels(labels),
		Operation: "event",
		Value:     1,
	}
	m.Labels["event_type"] = m.Name
	s.Push(m)
}

func (s *Server) Push(metric Metric) {
	var (
		err        error
		metricData []byte
	)
	metricData, err = json.Marshal(metric)
	if err != nil {
		log.Errorf("Cannot encode metric to json, %v", err)
	}
	log.Debugf("Push metric: %v", string(metricData))
	_, err = s.Connection.Write(metricData)
	if err != nil {
		log.Errorf("Cannot push metric to metrics-server, %v", err)
	}
	metric.Labels = nil
}

func sliceContains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

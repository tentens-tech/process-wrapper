package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
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
	SidecarVMAgentPushErrorsName                 = "sidecar_vmagent_push_errors_total"
	SidecarPhpScopeRestartsName                  = "sidecar_phpscope_restarts_total"
	SidecarCmdRestartBackoffSecondsName          = "sidecar_cmd_restart_backoff_seconds"
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
	SidecarVMAgentPushErrorsTotal            prometheus.Counter
	SidecarPhpScopeRestartsTotal             *prometheus.CounterVec
	SidecarCmdRestartBackoffSeconds          *prometheus.GaugeVec
)

// PrometheusHandler registers /metrics on mux
func PrometheusHandler(mux *http.ServeMux) {
	mux.Handle("/metrics", promhttp.Handler())
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
	SidecarVMAgentPushErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: SidecarVMAgentPushErrorsName,
		Help: "Total number of failed HTTP pushes to VMAgent for preempted metric",
	})
	SidecarPhpScopeRestartsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: SidecarPhpScopeRestartsName,
		Help: "Number of times the phpscope subprocess was restarted by process-wrapper",
	}, []string{"reason"})
	SidecarCmdRestartBackoffSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: SidecarCmdRestartBackoffSecondsName,
		Help: "Current restart back-off delay (seconds) before the next restart of the sidecar command. 0 = no pending back-off (process running or counter reset)",
	}, []string{"process_name"})
	prometheus.MustRegister(
		SidecarAcquireLockTotal,
		SidecarCmdReadinessCount,
		SidecarCmdDurationSecondsGauge,
		SidecarCmdCPUSeconds,
		SidecarCmdCPUSecondsGauge,
		SidecarCmdMemoryUsageGauge,
		SidecarCmdHeartBeatPeriodGauge,
		SidecarMetricsMergeDurationSecondsBucket,
		SidecarVMAgentPushErrorsTotal,
		SidecarPhpScopeRestartsTotal,
		SidecarCmdRestartBackoffSeconds,
	)
}

var extraJSONLabels map[string]string
var extraFileLabels map[string]string

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
		labelsFile, err = os.Open(extraLabelsFilePath) // #nosec G304 -- path is provided by the operator via env
		if err != nil {
			log.Errorf("Cannot read the labels file %v, %v", extraLabelsFilePath, err)
			return
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
			if err != nil {
				if err != io.EOF {
					log.Errorf("Failed to read the labels file %v, %v", extraLabelsFilePath, err)
				}
				break
			}
		}
	}
}

// extraLabelsMu guards the lazy loading and reading of the extra labels:
// events are emitted from several goroutines
var extraLabelsMu sync.Mutex

// loadExtraLabels loads METRICS_JSON_EXTRA_LABELS and the labels file once
func loadExtraLabels() {
	extraLabelsMu.Lock()
	defer extraLabelsMu.Unlock()
	GetExtraJSONLabels()
	GetExtraFileLabels()
}

func MergeLabels(merge map[string]string) map[string]string {
	labels := map[string]string{
		"pod":       os.Getenv("HOSTNAME"),
		"namespace": os.Getenv("POD_NAMESPACE"),
		"node":      os.Getenv("NODE_NAME"),
	}

	loadExtraLabels()
	extraLabelsMu.Lock()
	for k, v := range extraJSONLabels {
		labels[k] = v
	}
	for k, v := range extraFileLabels {
		labels[k] = v
	}
	extraLabelsMu.Unlock()

	for k, v := range merge {
		labels[k] = v
	}
	return labels
}

func PushTerminationEvent(labels map[string]string) {
	emitEvent("termination", labels)
}

func PushHealthEvent(labels map[string]string) {
	if status, ok := labels["status"]; ok && status == "true" && log.GetLevel() != log.DebugLevel {
		return
	}
	emitEvent("readiness", labels)
}

func PushCommandRestartEvent(labels map[string]string) {
	emitEvent("restart", labels)
}

func formatPrometheusLine(name string, labels map[string]string, value float64) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(labels[k]))
		sb.WriteByte('"')
	}
	sb.WriteString("} ")
	sb.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	sb.WriteByte('\n')
	return sb.String()
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

type VMAgentPusher struct {
	url    string
	client *http.Client
}

func NewVMAgentPusher() *VMAgentPusher {
	loadExtraLabels()

	// Opt-in: without VMAGENT_PUSH_URL nothing is pushed
	url := os.Getenv("VMAGENT_PUSH_URL")
	return &VMAgentPusher{
		url: url,
		client: &http.Client{
			Timeout: 1 * time.Second,
		},
	}
}

func (p *VMAgentPusher) PushPreempted(value float64) {
	if p == nil || p.url == "" {
		return
	}
	labels := MergeLabels(nil)
	labels["event_type"] = "preempted"

	body := formatPrometheusLine("preempted", labels, value)

	req, err := http.NewRequest(http.MethodPost, p.url, strings.NewReader(body))
	if err != nil {
		log.Errorf("vmagent push: build request: %v", err)
		SidecarVMAgentPushErrorsTotal.Inc()
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := p.client.Do(req)
	if err != nil {
		log.Errorf("vmagent push: request: %v", err)
		SidecarVMAgentPushErrorsTotal.Inc()
		return
	}
	defer resp.Body.Close()
	// Drain the body so the keep-alive connection is reused
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		log.Errorf("vmagent push: unexpected status %d", resp.StatusCode)
		SidecarVMAgentPushErrorsTotal.Inc()
	}
}

func sliceContains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

// emitEvent writes a single JSON line to stdout representing a process-wrapper
// lifecycle event. Grafana Alloy matches lines by the "_emitter" top-level
// field and promotes labels.event_type / labels.signal / labels.version to
// Loki stream labels. Failures are logged at WARN and swallowed — stdout is
// never retried.
func emitEvent(name string, extraLabels map[string]string) {
	labels := MergeLabels(extraLabels)
	labels["event_type"] = name
	payload := struct {
		Emitter   string            `json:"_emitter"`
		Name      string            `json:"name"`
		Labels    map[string]string `json:"labels"`
		Operation string            `json:"op"`
		Value     float64           `json:"value"`
	}{
		Emitter:   "process-wrapper",
		Name:      name,
		Labels:    labels,
		Operation: "event",
		Value:     1,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Warnf("emit event %s: marshal: %v", name, err)
		return
	}
	fmt.Fprintln(os.Stdout, string(data))
}

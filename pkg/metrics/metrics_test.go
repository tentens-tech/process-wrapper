package metrics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	log "github.com/sirupsen/logrus"
)

// Tests do NOT use t.Parallel() — extraJSONLabels and extraFileLabels are
// package-level globals that get cached on first read. Running tests in
// parallel would interleave state and produce flaky results. Every test that
// mutates env-driven labels must call resetExtraLabels() first.

func resetExtraLabels() {
	extraJSONLabels = nil
	extraFileLabels = nil
}

func TestFormatPrometheusLine_DeterministicOrder(t *testing.T) {
	labels := map[string]string{
		"app":         "adserver-api",
		"namespace":   "adserver",
		"pod":         "adserver-api-abc",
		"node":        "gke-node-1",
		"event_type":  "preempted",
		"version":     "0.0.74",
		"k8s_cluster": "prod-6",
	}

	first := formatPrometheusLine("preempted", labels, 1)
	for i := 0; i < 10; i++ {
		again := formatPrometheusLine("preempted", labels, 1)
		if again != first {
			t.Fatalf("non-deterministic output:\n  iter 0: %q\n  iter %d: %q", first, i+1, again)
		}
	}
}

func TestFormatPrometheusLine_EscapesLabelValues(t *testing.T) {
	labels := map[string]string{
		"quote":     `has "quote"`,
		"backslash": `has \backslash`,
		"newline":   "has\nnewline",
	}

	out := formatPrometheusLine("test_metric", labels, 1)

	wantSubstrings := []string{
		`backslash="has \\backslash"`,
		`newline="has\nnewline"`,
		`quote="has \"quote\""`,
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(out, sub) {
			t.Errorf("output %q does not contain expected substring %q", out, sub)
		}
	}
}

func TestFormatPrometheusLine_PreemptedPayload(t *testing.T) {
	labels := map[string]string{
		"app":         "adserver-api",
		"namespace":   "adserver",
		"pod":         "adserver-api-bcf7f7fd8-c7929",
		"node":        "gke-prod-6-api-spot-hmem-usc1-c-v2-bc51a1fd-7cjb",
		"event_type":  "preempted",
		"version":     "0.0.74",
		"k8s_cluster": "prod-6",
	}

	got := formatPrometheusLine("preempted", labels, 1)

	want := `preempted{app="adserver-api",event_type="preempted",k8s_cluster="prod-6",namespace="adserver",node="gke-prod-6-api-spot-hmem-usc1-c-v2-bc51a1fd-7cjb",pod="adserver-api-bcf7f7fd8-c7929",version="0.0.74"} 1` + "\n"

	if got != want {
		t.Errorf("golden payload mismatch\n  got:  %q\n  want: %q", got, want)
	}
}

func TestVMAgentPusher_PushPreempted_Success(t *testing.T) {
	resetExtraLabels()

	var gotBody string
	var gotMethod string
	var gotContentType string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	t.Setenv("VMAGENT_PUSH_URL", ts.URL)
	t.Setenv("HOSTNAME", "test-pod")
	t.Setenv("POD_NAMESPACE", "test-ns")
	t.Setenv("NODE_NAME", "test-node")

	pusher := NewVMAgentPusher()

	errsBefore := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)
	pusher.PushPreempted(1)
	errsAfter := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)

	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if !strings.HasPrefix(gotContentType, "text/plain") {
		t.Errorf("content-type: got %q want text/plain...", gotContentType)
	}
	if !strings.Contains(gotBody, "preempted{") {
		t.Errorf("body does not contain preempted{: %q", gotBody)
	}
	if !strings.Contains(gotBody, `pod="test-pod"`) {
		t.Errorf("body does not contain pod=test-pod: %q", gotBody)
	}
	if !strings.Contains(gotBody, `namespace="test-ns"`) {
		t.Errorf("body does not contain namespace=test-ns: %q", gotBody)
	}
	if !strings.Contains(gotBody, `node="test-node"`) {
		t.Errorf("body does not contain node=test-node: %q", gotBody)
	}
	if !strings.HasSuffix(gotBody, " 1\n") {
		t.Errorf("body does not end with value 1: %q", gotBody)
	}
	if errsAfter != errsBefore {
		t.Errorf("error counter changed: before=%v after=%v", errsBefore, errsAfter)
	}
}

func TestVMAgentPusher_PushPreempted_NetworkError(t *testing.T) {
	resetExtraLabels()

	// Port 1 on localhost is reserved and refuses connections synchronously.
	t.Setenv("VMAGENT_PUSH_URL", "http://127.0.0.1:1/api/v1/import/prometheus")
	t.Setenv("HOSTNAME", "test-pod")
	t.Setenv("POD_NAMESPACE", "test-ns")
	t.Setenv("NODE_NAME", "test-node")

	pusher := NewVMAgentPusher()

	errsBefore := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)
	pusher.PushPreempted(1)
	errsAfter := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)

	if delta := errsAfter - errsBefore; delta != 1 {
		t.Errorf("error counter delta: got %v want 1", delta)
	}
}

func TestVMAgentPusher_PushPreempted_HTTP500(t *testing.T) {
	resetExtraLabels()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	t.Setenv("VMAGENT_PUSH_URL", ts.URL)
	t.Setenv("HOSTNAME", "test-pod")
	t.Setenv("POD_NAMESPACE", "test-ns")
	t.Setenv("NODE_NAME", "test-node")

	pusher := NewVMAgentPusher()

	errsBefore := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)
	pusher.PushPreempted(1)
	errsAfter := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)

	if delta := errsAfter - errsBefore; delta != 1 {
		t.Errorf("error counter delta: got %v want 1", delta)
	}
}

func TestVMAgentPusher_PushPreempted_Timeout(t *testing.T) {
	resetExtraLabels()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	t.Setenv("VMAGENT_PUSH_URL", ts.URL)
	t.Setenv("HOSTNAME", "test-pod")
	t.Setenv("POD_NAMESPACE", "test-ns")
	t.Setenv("NODE_NAME", "test-node")

	pusher := NewVMAgentPusher()

	errsBefore := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)
	start := time.Now()
	pusher.PushPreempted(1)
	elapsed := time.Since(start)
	errsAfter := testutil.ToFloat64(SidecarVMAgentPushErrorsTotal)

	if elapsed >= 1500*time.Millisecond {
		t.Errorf("PushPreempted blocked too long: %v (want < 1.5s)", elapsed)
	}
	if delta := errsAfter - errsBefore; delta != 1 {
		t.Errorf("error counter delta: got %v want 1", delta)
	}
}

func TestVMAgentPusher_UsesMergeLabels(t *testing.T) {
	resetExtraLabels()

	// Temp labels file, downward-API style.
	dir := t.TempDir()
	labelsFile := filepath.Join(dir, "labels")
	content := "app=\"adserver-api\"\nversion=\"0.0.74\"\npod-template-hash=\"abc123\"\n"
	if err := os.WriteFile(labelsFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write labels file: %v", err)
	}

	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	t.Setenv("VMAGENT_PUSH_URL", ts.URL)
	t.Setenv("HOSTNAME", "adserver-api-bcf7f7fd8-c7929")
	t.Setenv("POD_NAMESPACE", "adserver")
	t.Setenv("NODE_NAME", "gke-prod-6-node")
	t.Setenv("METRICS_JSON_EXTRA_LABELS", `{"k8s_cluster":"prod-6"}`)
	t.Setenv("METRICS_EXTRA_LABELS_FILE_PATH", labelsFile)
	t.Setenv("METRICS_EXTRA_LABELS_FILE_EXCLUDE", "pod-template-hash")

	pusher := NewVMAgentPusher()
	pusher.PushPreempted(1)

	wantSubstrings := []string{
		`app="adserver-api"`,
		`namespace="adserver"`,
		`pod="adserver-api-bcf7f7fd8-c7929"`,
		`node="gke-prod-6-node"`,
		`event_type="preempted"`,
		`version="0.0.74"`,
		`k8s_cluster="prod-6"`,
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(gotBody, sub) {
			t.Errorf("body does not contain %q\nbody: %q", sub, gotBody)
		}
	}

	// Verify the excluded label is NOT present.
	if strings.Contains(gotBody, "pod-template-hash") || strings.Contains(gotBody, "pod_template_hash") {
		t.Errorf("excluded label leaked into body: %q", gotBody)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written. Restores os.Stdout via t.Cleanup. Tests that use this
// must NOT run in parallel — os.Stdout is a global.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

func TestEmitEvent_JSONShape(t *testing.T) {
	resetExtraLabels()

	t.Setenv("HOSTNAME", "test-pod")
	t.Setenv("POD_NAMESPACE", "test-ns")
	t.Setenv("NODE_NAME", "test-node")

	out := captureStdout(t, func() {
		emitEvent("termination", map[string]string{"signal": "kill"})
	})

	out = strings.TrimRight(out, "\n")
	if strings.Count(out, "\n") != 0 {
		t.Fatalf("expected exactly one line, got: %q", out)
	}

	var got struct {
		Emitter   string            `json:"_emitter"`
		Name      string            `json:"name"`
		Labels    map[string]string `json:"labels"`
		Operation string            `json:"op"`
		Value     float64           `json:"value"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\nline: %q", err, out)
	}

	if got.Emitter != "process-wrapper" {
		t.Errorf("_emitter: got %q want %q", got.Emitter, "process-wrapper")
	}
	if got.Name != "termination" {
		t.Errorf("name: got %q want %q", got.Name, "termination")
	}
	if got.Operation != "event" {
		t.Errorf("op: got %q want %q", got.Operation, "event")
	}
	if got.Value != 1 {
		t.Errorf("value: got %v want 1", got.Value)
	}
	if got.Labels["event_type"] != "termination" {
		t.Errorf("labels.event_type: got %q want %q", got.Labels["event_type"], "termination")
	}
	if got.Labels["signal"] != "kill" {
		t.Errorf("labels.signal: got %q want %q", got.Labels["signal"], "kill")
	}
	if got.Labels["pod"] != "test-pod" {
		t.Errorf("labels.pod: got %q want %q", got.Labels["pod"], "test-pod")
	}
	if got.Labels["namespace"] != "test-ns" {
		t.Errorf("labels.namespace: got %q want %q", got.Labels["namespace"], "test-ns")
	}
	if got.Labels["node"] != "test-node" {
		t.Errorf("labels.node: got %q want %q", got.Labels["node"], "test-node")
	}
}

func TestPushTerminationEvent_WritesToStdout(t *testing.T) {
	resetExtraLabels()

	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_NAMESPACE", "ns-1")
	t.Setenv("NODE_NAME", "node-1")

	out := captureStdout(t, func() {
		PushTerminationEvent(map[string]string{"signal": "termination"})
	})

	if !strings.Contains(out, `"_emitter":"process-wrapper"`) {
		t.Errorf("missing _emitter marker: %q", out)
	}
	if !strings.Contains(out, `"name":"termination"`) {
		t.Errorf("missing name=termination: %q", out)
	}
	if !strings.Contains(out, `"event_type":"termination"`) {
		t.Errorf("missing event_type=termination: %q", out)
	}
	if !strings.Contains(out, `"signal":"termination"`) {
		t.Errorf("missing signal=termination: %q", out)
	}
}

func TestPushHealthEvent_StatusTrue_NonDebug_Silent(t *testing.T) {
	resetExtraLabels()

	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_NAMESPACE", "ns-1")
	t.Setenv("NODE_NAME", "node-1")

	origLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(origLevel) })

	out := captureStdout(t, func() {
		PushHealthEvent(map[string]string{
			"status":       "true",
			"process_name": "svc",
			"probe":        "http",
		})
	})

	if out != "" {
		t.Errorf("expected empty stdout for status=true non-debug, got: %q", out)
	}
}

func TestPushHealthEvent_StatusFalse_Emits(t *testing.T) {
	resetExtraLabels()

	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_NAMESPACE", "ns-1")
	t.Setenv("NODE_NAME", "node-1")

	out := captureStdout(t, func() {
		PushHealthEvent(map[string]string{
			"status":       "false",
			"process_name": "svc",
			"probe":        "http",
		})
	})

	if !strings.Contains(out, `"event_type":"readiness"`) {
		t.Errorf("missing event_type=readiness: %q", out)
	}
	if !strings.Contains(out, `"status":"false"`) {
		t.Errorf("missing status=false: %q", out)
	}
}

func TestPushHealthEvent_StatusTrue_DebugLevel_Emits(t *testing.T) {
	resetExtraLabels()

	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_NAMESPACE", "ns-1")
	t.Setenv("NODE_NAME", "node-1")

	origLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() { log.SetLevel(origLevel) })

	out := captureStdout(t, func() {
		PushHealthEvent(map[string]string{
			"status":       "true",
			"process_name": "svc",
			"probe":        "http",
		})
	})

	if !strings.Contains(out, `"event_type":"readiness"`) {
		t.Errorf("missing event_type=readiness: %q", out)
	}
}

func TestPushCommandRestartEvent_WritesToStdout(t *testing.T) {
	resetExtraLabels()

	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_NAMESPACE", "ns-1")
	t.Setenv("NODE_NAME", "node-1")

	out := captureStdout(t, func() {
		PushCommandRestartEvent(map[string]string{
			"command":      "sh -c exec /app",
			"process_name": "svc",
		})
	})

	if !strings.Contains(out, `"name":"restart"`) {
		t.Errorf("missing name=restart: %q", out)
	}
	if !strings.Contains(out, `"event_type":"restart"`) {
		t.Errorf("missing event_type=restart: %q", out)
	}
	if !strings.Contains(out, `"process_name":"svc"`) {
		t.Errorf("missing process_name=svc: %q", out)
	}
}

// Events carry the extra labels without a VMAgent pusher (no preemption handling)
func TestMergeLabelsLoadsExtraLabels(t *testing.T) {
	resetExtraLabels()
	t.Cleanup(resetExtraLabels)
	t.Setenv("METRICS_JSON_EXTRA_LABELS", `{"team":"core"}`)
	if got := MergeLabels(nil)["team"]; got != "core" {
		t.Fatalf("extra label team = %q, want core", got)
	}
}

package metrics

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func resetLabels() {
	metricsServer, extraJSONLabels, extraFileLabels = nil, nil, nil
}

func TestEventLabels(t *testing.T) {
	resetLabels()
	defer resetLabels()
	labelsFile := filepath.Join(t.TempDir(), "labels")
	// downward API format
	err := os.WriteFile(labelsFile, []byte("app-name=\"web\"\npod-template-hash=\"abc\"\nsecurity.istio.io/tlsMode=\"istio\"\nteam=\"core\""), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_NAMESPACE", "ns")
	t.Setenv("NODE_NAME", "node-1")
	t.Setenv("METRICS_JSON_EXTRA_LABELS", `{"k8s_cluster":"test","team":"json"}`)
	t.Setenv("METRICS_EXTRA_LABELS_FILE_PATH", labelsFile)
	t.Setenv("METRICS_EXTRA_LABELS_FILE_EXCLUDE", "pod-template-hash,security.istio.io/tlsMode")

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s := NewMetricsServer(conn.LocalAddr().String())
	if s == nil {
		t.Fatal("metrics server is not created")
	}
	s.PushCommandRestartEvent(map[string]string{"process_name": "app", "team": "event"})

	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	var m Metric
	if err = json.Unmarshal(buf[:n], &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"pod": "pod-1", "namespace": "ns", "node": "node-1", // from env
		"k8s_cluster":  "test", // METRICS_JSON_EXTRA_LABELS
		"app_name":     "web",  // labels file, dashes replaced, quotes removed
		"team":         "event",
		"process_name": "app",
		"event_type":   "restart",
	}
	if m.Name != "restart" || m.Operation != "event" || !reflect.DeepEqual(m.Labels, want) {
		t.Errorf("unexpected event %+v", m)
	}
}

func TestNoMetricsServer(t *testing.T) {
	resetLabels()
	if s := NewMetricsServer(""); s != nil {
		t.Fatal("metrics server must be disabled without address")
	}
	// all pushes are no-ops on a disabled server
	var s *Server
	s.PushCommandRestartEvent(nil)
	s.PushTerminationEvent(nil)
	s.PushPreemptedEvent()
	s.PushInitPreemptedMetric()
	s.PushHealthEvent(map[string]string{"status": "false"})
}

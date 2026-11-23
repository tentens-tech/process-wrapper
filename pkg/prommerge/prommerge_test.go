package prommerge

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collect(t *testing.T, opts Options, targets []Target) (string, error) {
	t.Helper()
	families, err := New(opts).Collect(context.Background(), targets)
	var buf bytes.Buffer
	if wErr := Write(&buf, families); wErr != nil {
		t.Fatalf("Write: %v", wErr)
	}
	return buf.String(), err
}

func target(name, url string) Target {
	return Target{Name: name, URL: url, ExtraLabels: map[string]string{"process_name": name}}
}

const appA = `# HELP http_requests_total Total requests.
# TYPE http_requests_total counter
http_requests_total{code="200",path="/a,b"} 10
http_requests_total{code="500",path="say \"hi\""} 1
# HELP temp Temperature.
# TYPE temp gauge
temp NaN
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.1"} 1
latency_seconds_bucket{le="+Inf"} 3
latency_seconds_sum 0.7
latency_seconds_count 3
`

const appB = `# HELP http_requests_total Total requests.
# TYPE http_requests_total counter
http_requests_total{code="200",process_name="spoofed"} 5 1700000000000
# TYPE temp gauge
temp +Inf
`

func TestCollectMergesTargets(t *testing.T) {
	a, b := serve(t, appA), serve(t, appB)
	out, err := collect(t, Options{Sort: true, Async: true}, []Target{target("a", a.URL), target("b", b.URL)})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, want := range []string{
		`# HELP http_requests_total Total requests.`,
		`http_requests_total{code="200",path="/a,b",process_name="a"} 10`,
		`http_requests_total{code="500",path="say \"hi\"",process_name="a"} 1`,
		// extra label overrides the one exposed by the target; timestamp is kept
		`http_requests_total{code="200",process_name="b"} 5 1700000000000`,
		`temp{process_name="a"} NaN`,
		`temp{process_name="b"} +Inf`,
		`latency_seconds_bucket{process_name="a",le="+Inf"} 3`,
		`latency_seconds_count{process_name="a"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
	if n := strings.Count(out, "# TYPE http_requests_total counter"); n != 1 {
		t.Errorf("expected TYPE once, got %d\n%s", n, out)
	}
	if strings.Contains(out, "spoofed") {
		t.Errorf("target label was not overridden\n%s", out)
	}
}

func TestCollectSkipsFailedTarget(t *testing.T) {
	a := serve(t, appA)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	out, err := collect(t, Options{}, []Target{target("a", a.URL), target("bad", bad.URL)})
	if err == nil {
		t.Fatal("expected error for failed target")
	}
	if !strings.Contains(out, `process_name="a"`) {
		t.Errorf("healthy target is missing\n%s", out)
	}

	out, err = collect(t, Options{EmptyOnFailure: true}, []Target{target("a", a.URL), target("bad", bad.URL)})
	if err == nil || out != "" {
		t.Errorf("expected empty output and error, got %q, %v", out, err)
	}
}

func TestCollectRejectsInvalidAndOversizedBodies(t *testing.T) {
	invalid := serve(t, "not a metric line {{{\n")
	big := serve(t, strings.Repeat("x 1\n", 1000))

	_, err := collect(t, Options{}, []Target{target("invalid", invalid.URL)})
	if err == nil {
		t.Error("expected parse error")
	}
	_, err = collect(t, Options{MaxResponseBytes: 100}, []Target{target("big", big.URL)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected size limit error, got %v", err)
	}
}

func TestCollectTypeConflictAndOmitMeta(t *testing.T) {
	a := serve(t, "# HELP m help\n# TYPE m counter\nm 1\n")
	b := serve(t, "# TYPE m gauge\nm 2\n")

	out, err := collect(t, Options{OmitMeta: true}, []Target{target("a", a.URL), target("b", b.URL)})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if strings.Contains(out, "# HELP") {
		t.Errorf("HELP must be omitted\n%s", out)
	}
	if !strings.Contains(out, `m{process_name="a"} 1`) || strings.Contains(out, `process_name="b"`) {
		t.Errorf("conflicting family must be skipped\n%s", out)
	}
}

package process

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	for path, want := range map[string]bool{"/ok": true, "/redirect": true, "/fail": false} {
		if got := (HTTPProbe{URL: srv.URL + path}).Run("app"); got != want {
			t.Errorf("%v: got %v, want %v", path, got, want)
		}
	}
	if (HTTPProbe{URL: "http://127.0.0.1:1/"}).Run("app") {
		t.Error("probe of a closed port must fail")
	}
}

func TestTCPProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if !(TCPProbe{Socket: addr}).Run("app") {
		t.Error("probe of a listening socket must pass")
	}
	_ = l.Close()
	if (TCPProbe{Socket: addr}).Run("app") {
		t.Error("probe of a closed socket must fail")
	}
}

func TestFileProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if (FileProbe{Path: path}).Run("app") {
		t.Error("missing file must fail")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !(FileProbe{Path: path}).Run("app") {
		t.Error("existing file must pass")
	}
}

func TestEnvProbe(t *testing.T) {
	t.Setenv("PROBE_ENV", "enabled")
	if !(EnvProbe{Name: "PROBE_ENV", Value: "enabled"}).Run("app") {
		t.Error("matching env must pass")
	}
	if (EnvProbe{Name: "PROBE_ENV", Value: "disabled"}).Run("app") {
		t.Error("different value must fail")
	}
	if (EnvProbe{Name: "PROBE_ENV_MISSING", Value: "enabled"}).Run("app") {
		t.Error("missing env must fail")
	}
}

func TestExecProbeExitCodeAndEnvs(t *testing.T) {
	p := ExecProbe{Command: Command{Entrypoint: "sh", Args: []string{"-c", `test "$EXPECTED" = ok`}}}
	if p.Run("app") {
		t.Error("probe without the expected env must fail")
	}
	p.Command.Envs = map[string]string{"EXPECTED": "ok"}
	if !p.Run("app") {
		t.Error("probe envs must be passed to the command")
	}
	if (ExecProbe{Command: Command{Entrypoint: "/nonexistent/binary"}}).Run("app") {
		t.Error("missing entrypoint must fail")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p = ExecProbe{Command: Command{Entrypoint: "test", Args: []string{"-f", "marker"}, Dir: dir}}
	if !p.For(&Command{Dir: "/"}).Run("app") {
		t.Error("probe dir must override the command dir")
	}
}

// floatingPid counts process starts (the first one too) within the period
func TestFloatingPidProbe(t *testing.T) {
	s := newTestState()
	p := FloatingPid{State: s, Period: Duration{time.Minute}, Threshold: 2}
	s.SetPid("app", 100)
	s.SetPid("app", 101)
	if !p.Run("app") {
		t.Error("starts within the threshold must pass")
	}
	s.SetPid("app", 102)
	if p.Run("app") {
		t.Error("starts over the threshold must fail")
	}
	// Defaults: 10s period, threshold 1 (any restart within 10s fails)
	s = newTestState()
	p = FloatingPid{State: s}
	s.SetPid("app", 1)
	if !p.Run("app") {
		t.Error("the first start must pass with the default threshold")
	}
	s.SetPid("app", 2)
	if p.Run("app") {
		t.Error("a restart must fail with the default threshold")
	}
}

func TestHeartBeatProbe(t *testing.T) {
	s := newTestState()
	p := &HeartBeat{State: s, Period: Duration{time.Minute}}
	if p.Run("app") {
		t.Error("probe without heartbeats must fail")
	}
	s.SetHeartBeat("app", time.Now())
	if !p.Run("app") {
		t.Error("recent heartbeat must pass")
	}
	s.SetHeartBeat("app", time.Now().Add(-2*time.Minute))
	if p.Run("app") {
		t.Error("expired heartbeat must fail")
	}
}

func TestHookEnvs(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	h := &Hook{
		Envs: map[string]string{"HOOK_ENV": "hook", "OVERRIDDEN": "hook"},
		Command: &Command{
			Entrypoint: "sh",
			Args:       []string{"-c", `echo "$PROCESS_ENV $HOOK_ENV $HOOK_CMD_ENV $OVERRIDDEN" > ` + out},
			Envs:       map[string]string{"HOOK_CMD_ENV": "cmd"},
		},
	}
	envs := map[string]string{"PATH": os.Getenv("PATH"), "PROCESS_ENV": "process", "OVERRIDDEN": "process"}
	if !h.Run(context.Background(), &Command{Name: "app"}, envs) {
		t.Fatal("hook failed")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "process hook cmd hook" {
		t.Errorf("unexpected hook envs %q", got)
	}
	if envs["OVERRIDDEN"] != "process" || envs["HOOK_ENV"] != "" {
		t.Error("process envs must not be modified by the hook")
	}
}

func TestHookDeadlineAndFailure(t *testing.T) {
	start := time.Now()
	h := &Hook{Deadline: Duration{500 * time.Millisecond}, Command: &Command{Entrypoint: "sleep", Args: []string{"10"}}}
	if h.Run(context.Background(), &Command{Name: "app"}, map[string]string{"PATH": os.Getenv("PATH")}) {
		t.Error("hook exceeding the deadline must fail")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("hook was not stopped at the deadline: %v", time.Since(start))
	}
	h = &Hook{Command: &Command{Entrypoint: "false"}}
	if h.Run(context.Background(), &Command{Name: "app"}, map[string]string{"PATH": os.Getenv("PATH")}) {
		t.Error("failed hook must fail")
	}
	if h.Deadline.Duration != DefaultHookDeadlineSeconds*time.Second {
		t.Errorf("unexpected default deadline %v", h.Deadline)
	}
	if !(*Hook)(nil).Run(context.Background(), &Command{Name: "app"}, nil) {
		t.Error("no hook must succeed")
	}
	h = &Hook{SQL: &SQLHook{Queries: []string{"SELECT 1"}}}
	if h.Run(context.Background(), &Command{Name: "app"}, map[string]string{}) {
		t.Error("sql hook without MYSQL_DSN must fail")
	}
}

func TestPodStatusStopsWrapperWhenMainContainerCompleted(t *testing.T) {
	for reason, want := range map[string]bool{"Completed": true, "Error": false} {
		canceled := false
		p := &PodStatus{cancel: func() { canceled = true }}
		p.Status.ContainerStatuses = []ContainerStatuses{
			{Name: "sidecar", State: map[string]ContainerState{"running": {}}},
			{Name: "main", State: map[string]ContainerState{"terminated": {Reason: reason}}},
		}
		p.handleStatus()
		if canceled != want {
			t.Errorf("%v: canceled %v, want %v", reason, canceled, want)
		}
	}
}

func TestDurationParsing(t *testing.T) {
	var d Duration
	for in, want := range map[string]time.Duration{`"1m30s"`: 90 * time.Second, `1000000000`: time.Second} {
		if err := d.UnmarshalJSON([]byte(in)); err != nil || d.Duration != want {
			t.Errorf("%v: got %v, %v", in, d.Duration, err)
		}
	}
	for _, in := range []string{`"5 seconds"`, `true`} {
		if err := d.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("%v must not be parsed", in)
		}
	}
	data, err := Duration{90 * time.Second}.MarshalJSON()
	if err != nil || string(data) != `"1m30s"` {
		t.Errorf("unexpected json %s, %v", data, err)
	}
}

// A failed postStart hook restarts a process without readiness probes too
func TestFailedHookRestartsProcessWithoutReadiness(t *testing.T) {
	s := newTestState()
	c := &Command{
		Name: "app", Entrypoint: "sleep", Args: []string{"30"}, s: s,
		TerminationGracePeriod: Duration{time.Second},
		PostStart:              &Hook{Command: &Command{Entrypoint: "false"}},
	}
	done := make(chan bool)
	go func() { done <- c.runOnce(context.Background()) }()
	select {
	case restart := <-done:
		if !restart {
			t.Error("the process must be restarted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process was not stopped after the hook failure")
	}
	if s.ProcessState()["app"] {
		t.Error("the process must not be ready")
	}
}

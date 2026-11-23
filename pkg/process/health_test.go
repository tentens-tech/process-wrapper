package process

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// newTestState builds a State without starting HTTP servers and background routines
func newTestState() *State {
	return &State{
		pidsMap:          make(map[string]int),
		statusMap:        make(map[string]bool),
		killedMap:        make(map[string]bool),
		floatingPid:      make(map[string][]time.Time),
		heartBeat:        make(map[string]time.Time),
		cancelRoutineMap: make(map[string]context.CancelFunc),
		preemptedEvent:   make(chan struct{}),
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %v", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReadinessStates(t *testing.T) {
	s := newTestState()
	s.SetState("app", false)
	if s.Readiness() {
		t.Fatal("not started process must not be ready")
	}
	// Liveness always passes: restarts are handled by the wrapper itself,
	// kubelet must not restart the whole pod because of a degraded process
	if !s.Liveness() {
		t.Fatal("liveness must always pass")
	}
	s.SetState("app", true)
	if !s.Readiness() || !s.Liveness() {
		t.Fatal("healthy process must be ready/live")
	}
	s.SetShutdown()
	if s.Readiness() {
		t.Fatal("readiness must fail during graceful shutdown")
	}
	if !s.Liveness() {
		t.Fatal("liveness must keep passing during graceful shutdown")
	}
}

func TestProbeRunnerRecoversAfterPostStartHook(t *testing.T) {
	s := newTestState()
	file := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hookRuns := filepath.Join(t.TempDir(), "hook")
	c := &Command{
		Name: "app",
		s:    s,
		Readiness: &Probe{
			Period:           Duration{10 * time.Millisecond},
			FailureThreshold: 1000,
			File:             &FileProbe{Path: file},
		},
		PostStart: &Hook{Command: &Command{Entrypoint: "sh", Args: []string{"-c", "echo x >> " + hookRuns}}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.ProbeRunner(ctx, cancel, log.WithField("test", t.Name()), 0, map[string]string{"PATH": os.Getenv("PATH")})

	ready := func() bool { return s.ProcessState()["app"] }
	waitFor(t, "ready", ready)

	_ = os.Remove(file)
	waitFor(t, "not ready", func() bool { return !ready() })

	_ = os.WriteFile(file, nil, 0o600)
	waitFor(t, "ready again after probe recovery", ready)

	data, _ := os.ReadFile(hookRuns)
	if string(data) != "x\n" {
		t.Errorf("postStart hook must run exactly once, got %q", data)
	}
}

func TestProbeRunnerRestartsAfterConsecutiveFailures(t *testing.T) {
	s := newTestState()
	proc, done := startTracked(t, s, "app", "sleep", "30")

	c := &Command{
		Name: "app",
		s:    s,
		Readiness: &Probe{
			Period:           Duration{10 * time.Millisecond},
			FailureThreshold: 3,
			File:             &FileProbe{Path: filepath.Join(t.TempDir(), "missing")},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ProbeRunner(ctx, cancel, log.WithField("test", t.Name()), proc.Process.Pid, nil)

	if !s.IsKilled("app") {
		t.Error("process must be marked as killed by the health checker")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = proc.Process.Kill()
		t.Fatal("process was not killed")
	}
}

// startTracked starts a process registered in the state like Run does:
// its pid is removed from the state once the process is reaped.
func startTracked(t *testing.T, s *State, name string, args ...string) (*exec.Cmd, chan struct{}) {
	t.Helper()
	proc := exec.Command(args[0], args[1:]...)
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	s.SetPid(name, proc.Process.Pid)
	done := make(chan struct{})
	go func() {
		_ = proc.Wait()
		s.RemovePid(name, proc.Process.Pid)
		close(done)
	}()
	t.Cleanup(func() { _ = proc.Process.Kill() })
	return proc, done
}

func TestTerminateProcessGraceful(t *testing.T) {
	s := newTestState()
	marker := filepath.Join(t.TempDir(), "terminated")
	// The shell handles SIGTERM and exits on its own
	proc, done := startTracked(t, s, "app", "sh", "-c", "trap 'touch "+marker+"; exit 0' TERM; while true; do sleep 0.1; done")
	time.Sleep(200 * time.Millisecond) // let the shell install the trap

	c := &Command{Name: "app", s: s, TerminationGracePeriod: Duration{5 * time.Second}}
	start := time.Now()
	c.terminateProcess(log.WithField("test", t.Name()), proc.Process.Pid)
	<-done
	if _, err := os.Stat(marker); err != nil {
		t.Error("process didn't receive SIGTERM before being stopped")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("termination waited for the whole grace period: %v", time.Since(start))
	}
}

func TestTerminateProcessKillsAfterGracePeriod(t *testing.T) {
	s := newTestState()
	proc, done := startTracked(t, s, "app", "sh", "-c", "trap '' TERM; while true; do sleep 0.1; done")
	time.Sleep(200 * time.Millisecond)

	c := &Command{Name: "app", s: s, TerminationGracePeriod: Duration{300 * time.Millisecond}}
	c.terminateProcess(log.WithField("test", t.Name()), proc.Process.Pid)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process ignoring SIGTERM was not killed")
	}
}

func TestHeartBeatDefaultPeriod(t *testing.T) {
	s := newTestState()
	h := &HeartBeat{State: s}
	if h.Run("app") {
		t.Error("probe without heartbeats must fail")
	}
	s.SetHeartBeat("app", time.Now().Add(-10*time.Second))
	if !h.Run("app") {
		t.Errorf("heartbeat 10s ago must pass with the default period %v", DefaultHeartBeatPeriod)
	}
	s.SetHeartBeat("app", time.Now().Add(-DefaultHeartBeatPeriod-time.Second))
	if h.Run("app") {
		t.Error("stale heartbeat must fail")
	}
}

func TestWaitHealthyStopsWhenClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/readiness?wait", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitHealthy(rec, req, func() bool { return false })
	}()
	cancel()
	wg.Wait()
	if rec.Code == http.StatusOK && rec.Body.Len() > 0 {
		t.Errorf("unexpected response %v", rec.Code)
	}

	rec = httptest.NewRecorder()
	waitHealthy(rec, httptest.NewRequest(http.MethodGet, "/readiness?wait", nil), func() bool { return true })
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %v", rec.Code)
	}
}

func TestLockProbeStatusCodes(t *testing.T) {
	for code, want := range map[int]bool{
		http.StatusOK:                  true,
		http.StatusNoContent:           false,
		http.StatusInternalServerError: false,
		http.StatusBadRequest:          false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		SharedLockURL = srv.URL
		if got := (LockProbe{Lease: "1"}).Run("app"); got != want {
			t.Errorf("status %v: expected %v, got %v", code, want, got)
		}
		srv.Close()
	}
}

func TestExecProbeInheritsEnvironment(t *testing.T) {
	p := ExecProbe{Command: Command{
		Entrypoint: "sh",
		Args:       []string{"-c", `test -n "$PATH" && test "$FOO" = bar`},
		Envs:       map[string]string{"FOO": "bar"},
	}}
	if !p.Run("app") {
		t.Error("exec probe must see PATH and probe envs")
	}
	if (ExecProbe{Command: Command{Entrypoint: "false"}}).Run("app") {
		t.Error("non-zero exit must fail the probe")
	}
}

func TestAMQPProbeMissingEnvDoesNotPanic(t *testing.T) {
	p := &AMQPProbe{Envs: map[string]string{}}
	if p.Run("app") {
		t.Error("probe without DSN must fail")
	}
}

func TestProbeCloneIsIndependent(t *testing.T) {
	orig := &Probe{SQL: &SQLProbe{Queries: []string{"SELECT 1"}}, AMQP: &AMQPProbe{}}
	cp := orig.clone()
	cp.SQL.Envs = map[string]string{"A": "b"}
	cp.Period.Duration = time.Second
	if orig.SQL.Envs != nil || orig.Period.Duration != 0 || cp.AMQP == orig.AMQP {
		t.Error("clone shares mutable state with the original")
	}
}

func TestRemovePidAndKnownProcess(t *testing.T) {
	s := newTestState()
	s.SetPid("app", 10)
	if !s.IsKnownProcess("app") || s.IsKnownProcess("other") {
		t.Fatal("unexpected known process state")
	}
	s.RemovePid("app", 11) // stale pid must not remove the current one
	if s.GetPid("app") != 10 {
		t.Fatal("pid removed by a stale value")
	}
	s.RemovePid("app", 10)
	if s.GetPid("app") != 0 {
		t.Fatal("pid not removed")
	}
}

func TestWithMetadataQuery(t *testing.T) {
	for in, want := range map[string]string{
		"http://m/preempted":     "http://m/preempted?wait_for_change=true&timeout_sec=55",
		"http://m/preempted?x=1": "http://m/preempted?x=1&wait_for_change=true&timeout_sec=55",
		"http://m/preempted?wait_for_change=true&timeout_sec=60": "http://m/preempted?wait_for_change=true&timeout_sec=60",
	} {
		if got := withMetadataQuery(in); got != want {
			t.Errorf("withMetadataQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

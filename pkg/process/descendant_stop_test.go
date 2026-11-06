package process

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// These tests are the spec for stopping a supervised command.
// They fail while a stop path signals only the direct child pid: a setsid
// descendant or a shell background job keeps running, the shared-lock
// keepalive has already stopped, and a later lease grant or a restarted
// command overlaps that descendant.
//
// A fix has to make every listed stop path reap that descendant before a new
// lease for the command is granted and before the next generation starts.
// Other supervised commands' descendants must be left running.

func TestMain(m *testing.M) {
	log.SetLevel(log.ErrorLevel)
	os.Exit(m.Run())
}

func TestDescendantsHealthKillSetsid(t *testing.T) {
	assertReapedBeforeNextLease(t, "stay-setsid", true)
}

func TestDescendantsHealthKillBackgroundJob(t *testing.T) {
	assertReapedBeforeNextLease(t, "stay-bg", true)
}

func TestDescendantsParentExitSetsid(t *testing.T) {
	assertReapedBeforeNextLease(t, "exit-setsid", false)
}

func TestDescendantsParentExitBackgroundJob(t *testing.T) {
	assertReapedBeforeNextLease(t, "exit-bg", false)
}

func TestDescendantsPostStartKillSetsid(t *testing.T) {
	requireLinux(t)
	gen := filepath.Join(t.TempDir(), "gen")
	ready := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(ready, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _, cancel := startSupervised(t, supervisedOpts{
		name: "poststart",
		mode: "stay-setsid",
		gen:  gen,
		readiness: &Probe{
			Delay:            Duration{200 * time.Millisecond},
			Period:           Duration{50 * time.Millisecond},
			FailureThreshold: 3,
			File:             &FileProbe{Path: ready},
		},
		postStart: &Hook{
			Deadline: Duration{time.Second},
			Command:  &Command{Entrypoint: "/bin/false"},
		},
	})
	defer cancel()
	direct, descendant := waitDirectAndDescendant(t, st, "poststart", gen)
	if !waitDead(direct, 5*time.Second) {
		t.Fatalf("postStart failure did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, nil, 5*time.Second)
}

func TestDescendantsOOMKillSetsid(t *testing.T) {
	requireLinux(t)
	gen := filepath.Join(t.TempDir(), "gen")
	// The first sample runs as soon as the process starts. Leave the limit off
	// until the setsid descendant has recorded itself, then arm it so the next
	// sample (within GetStatsPeriodSeconds) kills only the stored pid.
	st, cmd, cancel := startSupervised(t, supervisedOpts{
		name: "oom",
		mode: "stay-setsid",
		gen:  gen,
	})
	defer cancel()
	direct, descendant := waitDirectAndDescendant(t, st, "oom", gen)
	cmd.MemoryLimitMB = 0.0001
	if !waitDead(direct, 15*time.Second) {
		t.Fatalf("memory limit did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, nil, 3*time.Second)
}

func TestDescendantsCommandContextCancelSetsid(t *testing.T) {
	requireLinux(t)
	gen := filepath.Join(t.TempDir(), "gen")
	st, _, cancel := startSupervised(t, supervisedOpts{
		name: "ctx",
		mode: "stay-setsid",
		gen:  gen,
	})
	direct, descendant := waitDirectAndDescendant(t, st, "ctx", gen)
	// SIGINT's notifier and ProcessKiller both end here: the command context
	// is cancelled and CommandContext kills only cmd.Process.
	st.SetShutdown()
	cancel()
	if !waitDead(direct, 3*time.Second) {
		t.Fatalf("cancelling the command context did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, nil, 3*time.Second)
}

func TestDescendantsSetpgidBackgroundNotKilledWithParent(t *testing.T) {
	requireLinux(t)
	gen := filepath.Join(t.TempDir(), "gen")
	st, _, cancel := startSupervised(t, supervisedOpts{
		name: "pgroup",
		mode: "stay-bg",
		gen:  gen,
		sysProcAttr: &syscall.SysProcAttr{
			Setpgid: true,
		},
		readiness: &Probe{
			Delay:            Duration{200 * time.Millisecond},
			Period:           Duration{50 * time.Millisecond},
			FailureThreshold: 1,
			File:             &FileProbe{Path: filepath.Join(t.TempDir(), "missing-probe")},
		},
	})
	defer cancel()
	direct, descendant := waitDirectAndDescendant(t, st, "pgroup", gen)
	if !waitDead(direct, 6*time.Second) {
		t.Fatalf("health kill did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, nil, 3*time.Second)
}

func TestDescendantsHookCancelSetsid(t *testing.T) {
	requireLinux(t)
	gen := filepath.Join(t.TempDir(), "gen")
	script := descendantScript(t)
	hook := &Hook{
		Deadline: Duration{time.Minute},
		Command: &Command{
			Entrypoint: "/bin/bash",
			Args:       []string{script, gen, "stay-setsid"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- hook.Run(ctx, "hook", nil)
	}()
	descendant := waitPID(t, gen, time.Now().Add(3*time.Second))
	t.Cleanup(func() { killRecorded(gen) })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("hook did not return after its context was cancelled")
	}
	waitDescendantReaped(t, gen, descendant, nil, 3*time.Second)
}

func TestDescendantsExecProbeTimeoutSetsid(t *testing.T) {
	requireLinux(t)
	gen := filepath.Join(t.TempDir(), "gen")
	script := descendantScript(t)
	probe := ExecProbe{Command: Command{
		Entrypoint: "/bin/bash",
		Args:       []string{script, gen, "stay-setsid"},
	}}
	done := make(chan bool, 1)
	go func() {
		done <- probe.Run("exec-probe")
	}()
	descendant := waitPID(t, gen, time.Now().Add(3*time.Second))
	t.Cleanup(func() { killRecorded(gen) })
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("exec probe did not return")
	}
	waitDescendantReaped(t, gen, descendant, nil, 3*time.Second)
}

// assertReapedBeforeNextLease starts a supervised command and a bystander.
// The command either exits after daemonizing or fails its file probe.
// The descendant must be dead before another lease is granted and before a
// second generation is recorded. The bystander's descendant must stay up.
func assertReapedBeforeNextLease(t *testing.T, mode string, failHealth bool) {
	t.Helper()
	requireLinux(t)
	root := t.TempDir()
	gen := filepath.Join(root, "gen")
	bystanderGen := filepath.Join(root, "bystander")
	gate := newLeaseGate(t, gen)
	readiness := &Probe{
		Delay:            Duration{200 * time.Millisecond},
		Period:           Duration{50 * time.Millisecond},
		FailureThreshold: 1,
		KeepSharedLock:   &LockProbe{TTLDuration: "1s"},
	}
	if failHealth {
		readiness.File = &FileProbe{Path: filepath.Join(root, "missing-probe")}
	}
	st, _, cancel := startSupervised(t, supervisedOpts{
		name:      "worker",
		mode:      mode,
		gen:       gen,
		readiness: readiness,
	})
	defer cancel()
	_, _, byCancel := startSupervisedOn(t, st, nil, supervisedOpts{
		name: "bystander",
		mode: "stay-setsid",
		gen:  bystanderGen,
	})
	defer byCancel()

	direct, descendant := waitDirectAndDescendant(t, st, "worker", gen)
	bystander := waitPID(t, bystanderGen, time.Now().Add(3*time.Second))
	if failHealth && !waitDead(direct, 6*time.Second) {
		t.Fatalf("health kill did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, gate, 8*time.Second)
	if !processAlive(bystander) {
		t.Fatalf("stopping %s also killed bystander descendant %d", mode, bystander)
	}
}

type supervisedOpts struct {
	name          string
	mode          string
	gen           string
	readiness     *Probe
	postStart     *Hook
	memoryLimitMB float64
	sysProcAttr   *syscall.SysProcAttr
}

func startSupervised(t *testing.T, opts supervisedOpts) (*State, *Command, context.CancelFunc) {
	t.Helper()
	st := newTestState()
	cmd, _, cancel := startSupervisedOn(t, st, nil, opts)
	return st, cmd, cancel
}

func startSupervisedOn(t *testing.T, st *State, parent context.Context, opts supervisedOpts) (*Command, context.Context, context.CancelFunc) {
	t.Helper()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	script := descendantScript(t)
	cmd := &Command{
		Name:          opts.name,
		Entrypoint:    "/bin/bash",
		Args:          []string{script, opts.gen, opts.mode},
		Readiness:     opts.readiness,
		PostStart:     opts.postStart,
		MemoryLimitMB: opts.memoryLimitMB,
		SysProcAttr:   opts.sysProcAttr,
		s:             st,
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go cmd.Run(ctx, &wg)
	t.Cleanup(func() {
		st.SetShutdown()
		cancel()
		killRecorded(opts.gen)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(st.GetPid(opts.name)) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		killRecorded(opts.gen)
	})
	return cmd, ctx, cancel
}

func newTestState() *State {
	return &State{
		killedMap:        map[string]bool{},
		statusMap:        map[string]bool{},
		pidsMap:          map[string]int{},
		floatingPid:      map[string][]time.Time{},
		heartBeat:        map[string]time.Time{},
		cancelRoutineMap: map[string]context.CancelFunc{},
		preemptedEvent:   make(chan struct{}),
	}
}

func descendantScript(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "descendant.sh")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("descendant script: %v", err)
	}
	return src
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("descendant reaping is exercised on linux")
	}
}

func waitDirectAndDescendant(t *testing.T, st *State, name, gen string) (direct, descendant int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && direct == 0 {
		direct = st.GetPid(name)
		time.Sleep(5 * time.Millisecond)
	}
	if direct == 0 {
		t.Fatalf("supervised pid for %s was not recorded", name)
	}
	descendant = waitPID(t, gen, deadline)
	if descendant == direct {
		t.Fatalf("pid map recorded descendant %d; the case is not an orphan of the supervised pid", descendant)
	}
	return direct, descendant
}

func waitPID(t *testing.T, gen string, deadline time.Time) int {
	t.Helper()
	for time.Now().Before(deadline) {
		pids := readPIDs(gen)
		if len(pids) > 0 && processAlive(pids[0]) {
			return pids[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant pid was not recorded in %s", gen)
	return 0
}

func waitDescendantReaped(t *testing.T, gen string, descendant int, gate *leaseGate, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gate != nil {
			// Another replica acquires the same key once keepalive has stopped
			// and the TTL has elapsed. CreateLease is the supervisor's own client.
			if _, err := (&Command{Name: "worker"}).CreateLease("1s"); err != nil {
				t.Fatalf("lease acquire: %v", err)
			}
			if msg := gate.Violation(); msg != "" {
				t.Fatal(msg)
			}
		}
		pids := readPIDs(gen)
		if len(pids) > 1 && processAlive(descendant) {
			t.Fatalf("replacement descendant %d started while descendant %d from the previous run was still running (pids %v)", pids[len(pids)-1], descendant, pids)
		}
		if !processAlive(descendant) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant %d still running after %s (pids %v)", descendant, timeout, readPIDs(gen))
}

func waitDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAlive(pid)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndex(string(data), ")")
	if i < 0 || i+2 >= len(data) {
		return false
	}
	state := data[i+2]
	return state != 'Z' && state != 'X'
}

func readPIDs(gen string) []int {
	data, err := os.ReadFile(gen)
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func killRecorded(gen string) {
	for _, pid := range readPIDs(gen) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// leaseGate is the supervisor's lease HTTP contract from pkg/lease/handlers
// and pkg/lease/lock: 201 grants the key, 202 means it is still held, and a
// keepalive refreshes it. The key is dropped when the TTL elapses, which is
// when etcd revokes the lease and deletes the attached key.
type leaseGate struct {
	mu        sync.Mutex
	ttl       time.Duration
	expiry    time.Time
	id        int64
	held      bool
	genFile   string
	violation string
}

func newLeaseGate(t *testing.T, genFile string) *leaseGate {
	t.Helper()
	g := &leaseGate{ttl: time.Second, genFile: genFile}
	mux := http.NewServeMux()
	mux.HandleFunc("/lease", g.handleLease)
	mux.HandleFunc("/keepalive", g.handleKeepalive)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	SharedLockURL = srv.URL
	return g
}

func (g *leaseGate) Violation() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.violation
}

func (g *leaseGate) handleLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	ttl := g.ttl
	if parsed, err := time.ParseDuration(r.Header.Get("x-lease-ttl")); err == nil && parsed > 0 {
		ttl = parsed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.held && now.Before(g.expiry) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "0")
		return
	}
	g.held = true
	g.id++
	g.expiry = now.Add(ttl)
	g.ttl = ttl
	g.noteGrantLocked(body)
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, strconv.FormatInt(g.id, 10))
}

func (g *leaseGate) handleKeepalive(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	id, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil || !g.held || id != g.id || time.Now().After(g.expiry) {
		g.held = false
		w.WriteHeader(http.StatusNoContent)
		return
	}
	g.expiry = time.Now().Add(g.ttl)
	w.WriteHeader(http.StatusOK)
}

func (g *leaseGate) noteGrantLocked(body []byte) {
	if g.violation != "" {
		return
	}
	for _, pid := range readPIDs(g.genFile) {
		if processAlive(pid) {
			g.violation = fmt.Sprintf("lease granted while descendant %d was still running (body %s)", pid, strings.TrimSpace(string(body)))
			return
		}
	}
}

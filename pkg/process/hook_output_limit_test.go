package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// HookOutputCap is the most bytes of one command-hook stream (stdout, and
// separately stderr) the supervisor may retain or log. A hook that writes
// one byte more must be killed, the rest of that stream discarded, and only
// a truncated prefix logged. These tests are the spec for that bound: they
// fail while RunHook buffers an entire stream in memory.
const HookOutputCap = 64 * 1024

const (
	// oversizePayload is large enough that retaining it cannot be confused
	// with a capped prefix, and small enough to allocate safely in a test.
	oversizePayload = 8 << 20
	// hookMustStopWithin bounds how long an over-cap hook may run. The hook
	// deadline used below is longer, so waiting out the deadline is not enough.
	hookMustStopWithin = 5 * time.Second
	hookDeadline       = 20 * time.Second
	// maxHookAllocDelta is the most heap traffic RunHook may add while
	// collecting an over-cap stream. It allows several copies of a capped
	// prefix plus process-startup overhead, and it is far below oversizePayload.
	maxHookAllocDelta = uint64(HookOutputCap)*16 + (2 << 20)
)

func TestCommandHookStdoutAtCapSucceeds(t *testing.T) {
	res := runCommandHook(t, "stdout", HookOutputCap, 'Q')
	assertHookAccepted(t, res, 'Q', HookOutputCap)
}

func TestCommandHookStderrAtCapSucceeds(t *testing.T) {
	res := runCommandHook(t, "stderr", HookOutputCap, 'R')
	assertHookAccepted(t, res, 'R', HookOutputCap)
}

func TestCommandHookStdoutOverCapIsDiscardedAndHookKilled(t *testing.T) {
	assertHookRejected(t, runCommandHook(t, "stdout", oversizePayload, 'Q'), 'Q')
}

func TestCommandHookStderrOverCapIsDiscardedAndHookKilled(t *testing.T) {
	assertHookRejected(t, runCommandHook(t, "stderr", oversizePayload, 'R'), 'R')
}

func TestCommandHookStdoutAndStderrOverCapAreBothBounded(t *testing.T) {
	assertHookRejected(t, runCommandHook(t, "both", oversizePayload, 'S'), 'S')
}

func TestPostStartHookAfterReadyProbeBoundsOutput(t *testing.T) {
	// Attack path: probes pass, then PostStart runs the command hook.
	assertHookRejected(t, runPostStartHook(t, postStartAfterReadyProbe, "stdout", oversizePayload, 'T'), 'T')
}

func TestPostStartHookWithoutReadinessBoundsOutput(t *testing.T) {
	assertHookRejected(t, runPostStartHook(t, postStartNoReadiness, "stderr", oversizePayload, 'U'), 'U')
}

func TestPostStartHookWhenProbesUndefinedBoundsOutput(t *testing.T) {
	assertHookRejected(t, runPostStartHook(t, postStartProbesUndefined, "both", oversizePayload, 'V'), 'V')
}

type postStartMode int

const (
	postStartAfterReadyProbe postStartMode = iota
	postStartNoReadiness
	postStartProbesUndefined
)

type hookResult struct {
	accepted   bool
	elapsed    time.Duration
	logs       []string
	allocDelta uint64
	finished   bool
}

func runCommandHook(t *testing.T, stream string, n int, fill byte) hookResult {
	t.Helper()
	capLogs := installLogCapture(t)
	payload, done := payloadAndSentinel(t, n, fill)
	cmd := hookCommand(t, stream)
	ctx, cancel := context.WithTimeout(context.Background(), hookDeadline)
	defer cancel()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	accepted := cmd.RunHook(ctx, "worker", map[string]string{
		"HOOK_INPUT": payload,
		"HOOK_DONE":  done,
	})
	elapsed := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	finished := false
	if _, err := os.Stat(done); err == nil {
		finished = true
	}
	return hookResult{
		accepted:   accepted,
		elapsed:    elapsed,
		logs:       capLogs.messages(),
		allocDelta: after.TotalAlloc - before.TotalAlloc,
		finished:   finished,
	}
}

func runPostStartHook(t *testing.T, mode postStartMode, stream string, n int, fill byte) hookResult {
	t.Helper()
	capLogs := installLogCapture(t)
	payload, done := payloadAndSentinel(t, n, fill)
	worker, workerPid := startSleeper(t)

	c := &Command{
		Name:       "worker",
		Entrypoint: worker.path,
		Args:       []string{"120"},
		PostStart: &Hook{
			Deadline: Duration{Duration: hookDeadline},
			Command:  hookCommand(t, stream),
		},
		s: newTestState(),
	}
	envs := map[string]string{
		"HOOK_INPUT": payload,
		"HOOK_DONE":  done,
	}

	switch mode {
	case postStartAfterReadyProbe:
		ready := filepath.Join(t.TempDir(), "ready")
		if err := os.WriteFile(ready, []byte("ok"), 0o600); err != nil {
			t.Fatalf("write readiness file: %v", err)
		}
		c.Readiness = &Probe{
			Delay:            Duration{Duration: time.Millisecond},
			Period:           Duration{Duration: time.Hour},
			FailureThreshold: 3,
			File:             &FileProbe{Path: ready},
		}
	case postStartProbesUndefined:
		c.Readiness = &Probe{
			Delay:            Duration{Duration: time.Millisecond},
			Period:           Duration{Duration: time.Hour},
			FailureThreshold: 3,
		}
	case postStartNoReadiness:
	default:
		t.Fatalf("unknown postStart mode %d", mode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	go func() {
		c.HealthChecker(ctx, cancel, workerPid, envs)
		close(finished)
	}()

	healthReturned := false
	deadline := time.Now().Add(hookMustStopWithin)
	for time.Now().Before(deadline) {
		select {
		case <-finished:
			healthReturned = true
		default:
		}
		logged := maxPayloadBytes(capLogs.messages(), fill)
		// RunHook logs from a defer after the process exits, so a sentinel can
		// appear a moment before the retained bytes show up in the log.
		if logged > HookOutputCap {
			break
		}
		if healthReturned && logged > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)
	// Let a hook that already exited finish its deferred log before sampling.
	select {
	case <-finished:
		healthReturned = true
	case <-time.After(200 * time.Millisecond):
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	state, haveState := c.s.ProcessState()[c.Name]
	sentinel := false
	if _, err := os.Stat(done); err == nil {
		sentinel = true
	}
	// accepted is the supervisor's view of hook success. HealthChecker keeps
	// running after a successful postStart when a probe period is set, so a
	// sentinel plus a healthy state means the oversized write was allowed.
	accepted := haveState && state
	if !healthReturned && sentinel {
		accepted = true
	}
	_ = worker
	return hookResult{
		accepted:   accepted,
		elapsed:    elapsed,
		logs:       capLogs.messages(),
		allocDelta: after.TotalAlloc - before.TotalAlloc,
		finished:   sentinel,
	}
}

func assertHookAccepted(t *testing.T, res hookResult, fill byte, n int) {
	t.Helper()
	if !res.accepted || !res.finished {
		t.Fatalf("hook of %d bytes (at cap) was rejected (accepted=%v finished=%v elapsed=%s messages=%d)", n, res.accepted, res.finished, res.elapsed, len(res.logs))
	}
	if res.elapsed > hookMustStopWithin {
		t.Fatalf("hook of %d bytes took %s", n, res.elapsed)
	}
	got := maxPayloadBytes(res.logs, fill)
	if got != n {
		t.Fatalf("logged %d payload bytes, want the full %d-byte stream", got, n)
	}
}

func assertHookRejected(t *testing.T, res hookResult, fill byte) {
	t.Helper()
	got := maxPayloadBytes(res.logs, fill)
	var problems []string
	if res.accepted || res.finished {
		problems = append(problems, "hook was allowed to finish the oversized write")
	}
	if res.elapsed > hookMustStopWithin {
		problems = append(problems, "hook was not killed before the deadline")
	}
	if got == 0 {
		problems = append(problems, "log has no truncated prefix of the command output")
	}
	if got > HookOutputCap {
		problems = append(problems, "log retained more than the cap")
	}
	if res.allocDelta > maxHookAllocDelta {
		problems = append(problems, "heap allocations grew with the uncapped stream")
	}
	if len(problems) > 0 {
		t.Fatalf("%s; accepted=%v finished=%v elapsed=%s loggedPayload=%d allocDelta=%d cap=%d",
			joinProblems(problems), res.accepted, res.finished, res.elapsed, got, res.allocDelta, HookOutputCap)
	}
}

func joinProblems(problems []string) string {
	out := problems[0]
	for _, p := range problems[1:] {
		out += "; " + p
	}
	return out
}

func hookCommand(t *testing.T, stream string) *Command {
	t.Helper()
	catPath := lookTool(t, "cat")
	touchPath := lookTool(t, "touch")
	shPath := lookTool(t, "sh")
	var script string
	switch stream {
	case "stdout":
		script = catPath + ` "$HOOK_INPUT" && ` + touchPath + ` "$HOOK_DONE"`
	case "stderr":
		script = catPath + ` "$HOOK_INPUT" >&2 && ` + touchPath + ` "$HOOK_DONE"`
	case "both":
		// Stderr is written first so a cap that applies only to stdout still
		// retains the whole stderr stream before the process is stopped.
		script = catPath + ` "$HOOK_INPUT" >&2 && ` + catPath + ` "$HOOK_INPUT" && ` + touchPath + ` "$HOOK_DONE"`
	default:
		t.Fatalf("unknown stream %q", stream)
	}
	return &Command{
		Entrypoint: shPath,
		Args:       []string{"-c", script},
	}
}

func payloadAndSentinel(t *testing.T, n int, fill byte) (payloadPath, donePath string) {
	t.Helper()
	dir := t.TempDir()
	payloadPath = filepath.Join(dir, "payload")
	donePath = filepath.Join(dir, "done")
	if err := os.WriteFile(payloadPath, bytes.Repeat([]byte{fill}, n), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return payloadPath, donePath
}

func lookTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return p
}

type sleeper struct {
	path string
	cmd  *exec.Cmd
}

func startSleeper(t *testing.T) (*sleeper, int) {
	t.Helper()
	path := lookTool(t, "sleep")
	cmd := exec.Command(path, "120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return &sleeper{path: path, cmd: cmd}, cmd.Process.Pid
}

func newTestState() *State {
	return &State{
		statusMap:        make(map[string]bool),
		killedMap:        make(map[string]bool),
		pidsMap:          make(map[string]int),
		floatingPid:      make(map[string][]time.Time),
		heartBeat:        make(map[string]time.Time),
		cancelRoutineMap: make(map[string]context.CancelFunc),
	}
}

type logCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *logCapture) Levels() []log.Level { return log.AllLevels }

func (c *logCapture) Fire(e *log.Entry) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, e.Message)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func installLogCapture(t *testing.T) *logCapture {
	t.Helper()
	logger := log.StandardLogger()
	prevOut := logger.Out
	prevLevel := logger.GetLevel()
	prevHooks := logger.ReplaceHooks(make(log.LevelHooks))
	capLogs := &logCapture{}
	logger.AddHook(capLogs)
	logger.SetOutput(&discardWriter{})
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		logger.SetOutput(prevOut)
		logger.SetLevel(prevLevel)
		logger.ReplaceHooks(prevHooks)
	})
	return capLogs
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func maxPayloadBytes(msgs []string, fill byte) int {
	max := 0
	for _, m := range msgs {
		n := bytes.Count([]byte(m), []byte{fill})
		if n > max {
			max = n
		}
	}
	return max
}

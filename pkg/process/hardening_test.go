package process

import (
	"bufio"
	"context"
	_ "expvar" // registers /debug/vars on http.DefaultServeMux
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// Health socket must not expose handlers registered globally by dependencies
func TestHealthMuxIsolatedFromDefaultServeMux(t *testing.T) {
	s := newTestState()
	s.SetState("app", true)
	s.registerHealthHandlers(s.Mux())
	srv := httptest.NewServer(s.Mux())
	defer srv.Close()

	for path, want := range map[string]int{
		"/debug/vars":     http.StatusNotFound,
		"/debug/requests": http.StatusNotFound,
		"/readiness":      http.StatusOK,
		"/status":         http.StatusOK,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%v: expected %v, got %v", path, want, resp.StatusCode)
		}
	}
}

func TestReadOutputLineBoundsLongLines(t *testing.T) {
	long := strings.Repeat("x", MaxOutputLineBytes*2+10)
	r := bufio.NewReaderSize(strings.NewReader(long+"\nlast line without newline"), MaxOutputLineBytes)
	var parts []string
	for {
		out, err := readOutputLine(r)
		if out != "" {
			parts = append(parts, out)
		}
		if err != nil {
			break
		}
	}
	if len(parts) != 4 {
		t.Fatalf("expected 3 parts of the long line and the last line, got %d", len(parts))
	}
	for _, p := range parts[:3] {
		if len(p) > MaxOutputLineBytes {
			t.Errorf("part is longer than %v: %v", MaxOutputLineBytes, len(p))
		}
	}
	if strings.Join(parts[:3], "") != long+"\n" || parts[3] != "last line without newline" {
		t.Error("output was lost")
	}
}

func TestProcessOutputLogsLastLineWithoutNewline(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	c := &Command{Name: "app"}
	c.ProcessOutput(bufio.NewReader(strings.NewReader("first\nno newline at EOF")), 1)
	var msgs []string
	for _, e := range hook.AllEntries() {
		if e.Data["name"] == "app" && e.Level == log.InfoLevel {
			msgs = append(msgs, e.Message)
		}
	}
	if len(msgs) != 2 || msgs[1] != "no newline at EOF" {
		t.Errorf("unexpected output %q", msgs)
	}
}

func TestCommandHookOutputLimit(t *testing.T) {
	supervised := &Command{Name: "app"}
	env := map[string]string{"PATH": os.Getenv("PATH")}

	atCap := &Command{Entrypoint: "sh", Args: []string{"-c", "head -c " + strconv.Itoa(MaxHookOutputBytes) + " /dev/zero"}}
	if !atCap.RunHook(context.Background(), supervised, env) {
		t.Error("hook writing exactly the limit must succeed")
	}

	start := time.Now()
	flood := &Command{Entrypoint: "sh", Args: []string{"-c", "yes"}}
	if flood.RunHook(context.Background(), supervised, env) {
		t.Error("hook exceeding the output limit must fail")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("hook exceeding the limit was not killed in time: %v", time.Since(start))
	}
}

func TestCommandHookDescendantsAreKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	hook := &Command{Entrypoint: "sh", Args: []string{"-c", "sleep 300 & echo $! > " + pidFile}}
	if !hook.RunHook(context.Background(), &Command{Name: "app"}, map[string]string{"PATH": os.Getenv("PATH")}) {
		t.Fatal("hook failed")
	}
	waitDead(t, readPid(t, pidFile))
}

func TestStartFailureIsRetried(t *testing.T) {
	for name, c := range map[string]*Command{
		"missing entrypoint": {Entrypoint: "/nonexistent/binary"},
		"missing dir":        {Entrypoint: "true", Dir: "/nonexistent/dir"},
		"missing envs file":  {Entrypoint: "true", EnvsFile: "/nonexistent/envs"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestState()
			c.Name, c.s = "app", s
			s.SetState("app", true)
			if !c.runOnce(context.Background()) {
				t.Error("start failure must be retried")
			}
			if s.ProcessState()["app"] {
				t.Error("command that can't start must not be ready")
			}
			if c.startBackoff != StartRetryMinDelay {
				t.Errorf("unexpected backoff %v", c.startBackoff)
			}
		})
	}

	// No retries on shutdown
	s := newTestState()
	s.SetShutdown()
	c := &Command{Name: "app", Entrypoint: "/nonexistent/binary", s: s}
	if c.runOnce(context.Background()) {
		t.Error("start must not be retried during shutdown")
	}
}

func TestStartFailureAfterCancel(t *testing.T) {
	s := newTestState()
	c := &Command{Name: "app", Entrypoint: "/nonexistent/binary", s: s, startBackoff: StartRetryMaxDelay}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// canceled context: no retry and no sleep
	start := time.Now()
	if c.runOnce(ctx) {
		t.Error("start must not be retried after the context is canceled")
	}
	if time.Since(start) > time.Second {
		t.Error("canceled start must not wait for the backoff")
	}
}

// Stopping a command stops its descendants (shell background jobs) too
func TestTerminateProcessKillsProcessGroup(t *testing.T) {
	s := newTestState()
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := &Command{
		Name:                   "app",
		Entrypoint:             "sh",
		Args:                   []string{"-c", "sleep 300 & echo $! > " + pidFile + "; wait"},
		TerminationGracePeriod: Duration{5 * time.Second},
		s:                      s,
	}
	done := make(chan bool)
	go func() { done <- c.runOnce(context.Background()) }()

	waitFor(t, "process started", func() bool { return s.GetPid("app") != 0 })
	child := readPid(t, pidFile)

	s.SetShutdown()
	c.terminateProcess(log.WithField("test", t.Name()), s.GetPid("app"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("command was not stopped")
	}
	waitDead(t, child)
}

// A command that exits leaves no background jobs behind
func TestExitedCommandDescendantsAreKilled(t *testing.T) {
	s := newTestState()
	s.SetShutdown() // don't restart
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := &Command{Name: "app", Entrypoint: "sh", Args: []string{"-c", "sleep 300 & echo $! > " + pidFile}, s: s}
	c.runOnce(context.Background())
	waitDead(t, readPid(t, pidFile))
}

func TestChildProcAttr(t *testing.T) {
	a := childProcAttr(nil, nil)
	if !a.Setpgid || a.Credential != nil {
		t.Errorf("unexpected attributes %+v", a)
	}

	own := &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 0, Gid: 0}}
	supervised := &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}}
	a = childProcAttr(own, supervised)
	if a.Credential.Uid != 1000 || a.Credential.Gid != 1000 {
		t.Errorf("child must run with the supervised credential, got %+v", a.Credential)
	}
	if own.Credential.Uid != 0 || &a.Credential.Groups[0] == &supervised.Credential.Groups[0] {
		t.Error("configs must not be modified or shared")
	}

	a = groupProcAttr(&syscall.SysProcAttr{Setsid: true})
	if a.Setpgid || !a.Setsid {
		t.Error("configured session/group settings must be kept")
	}
}

func TestSignalGroupRejectsInvalidPid(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if err := SignalGroup(pid, 0); err == nil {
			t.Errorf("pid %v must be rejected", pid)
		}
	}
}

func TestExecProbeRunsInSupervisedDir(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := ExecProbe{Command: Command{Entrypoint: "test", Args: []string{"-f", "marker"}}}
	if p.Run("app") {
		t.Fatal("probe must not find the file outside of the command dir")
	}
	if !p.For(&Command{Name: "app", Dir: dir}).Run("app") {
		t.Error("probe must run in the supervised command dir")
	}
}

func readPid(t *testing.T, file string) int {
	t.Helper()
	var pid int
	waitFor(t, "pid file", func() bool {
		data, err := os.ReadFile(file)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	})
	return pid
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	waitFor(t, "descendant to be killed", func() bool { return syscall.Kill(pid, 0) != nil })
}

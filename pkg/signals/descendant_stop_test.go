package signals

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"process-wrapper/pkg/process"
)

// Graceful shutdown and SIGINT must stop descendants of every supervised
// command. TerminateProcess signals only pids stored from cmd.Process, and
// ProcessKiller cancels the command context, which kills only that direct
// child. These tests fail while a setsid descendant is still running after
// that sequence. An unrelated process the supervisor did not start must keep
// running.

func TestMain(m *testing.M) {
	log.SetLevel(log.ErrorLevel)
	if runtime.GOOS == "linux" {
		dir, err := os.MkdirTemp("", "pw-health")
		if err != nil {
			panic(err)
		}
		// NewState registers handlers on the process-wide mux, so it can be
		// constructed once. ":0" asks the kernel for a free TCP port.
		os.Setenv("HEALTH_SOCKET", "127.0.0.1:0")
		sharedState = process.NewState(filepath.Join(dir, "ready"), false)
	}
	os.Exit(m.Run())
}

var sharedState *process.State

func TestDescendantsGracefulShutdown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant reaping is exercised on linux")
	}
	gen := filepath.Join(t.TempDir(), "gen")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := sharedState
	cmd := supervised(t, st, "term-worker", gen)
	var wg sync.WaitGroup
	wg.Add(1)
	go cmd.Run(ctx, &wg)
	t.Cleanup(func() {
		cancel()
		killRecorded(gen)
	})

	outsider := startOutsider(t)
	direct, descendant := waitDirectAndDescendant(t, st, "term-worker", gen)
	n := NewNotifier(cancel, 200*time.Millisecond, st)
	// Same order as Notifier.Notify on SIGTERM / SIGQUIT.
	st.CancelRoutines()
	st.SetShutdown()
	n.TerminateProcess()
	if !processAlive(outsider) {
		t.Fatalf("graceful shutdown killed unrelated pid %d", outsider)
	}
	if !waitDead(direct, 3*time.Second) {
		t.Fatalf("graceful shutdown did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, 3*time.Second)
}

func TestDescendantsInterrupt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant reaping is exercised on linux")
	}
	gen := filepath.Join(t.TempDir(), "gen")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := sharedState
	cmd := supervised(t, st, "int-worker", gen)
	var wg sync.WaitGroup
	wg.Add(1)
	go cmd.Run(ctx, &wg)
	t.Cleanup(func() {
		st.SetShutdown()
		cancel()
		killRecorded(gen)
	})

	outsider := startOutsider(t)
	direct, descendant := waitDirectAndDescendant(t, st, "int-worker", gen)
	n := NewNotifier(cancel, time.Second, st)
	go n.Notify()
	// signal.Notify registers asynchronously. Wait until the process signal
	// mask is installed before delivering SIGINT to this process.
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if !processAlive(outsider) {
		t.Fatalf("SIGINT killed unrelated pid %d", outsider)
	}
	if !waitDead(direct, 3*time.Second) {
		t.Fatalf("SIGINT did not stop supervised pid %d", direct)
	}
	waitDescendantReaped(t, gen, descendant, 3*time.Second)
}

func supervised(t *testing.T, st *process.State, name, gen string) *process.Command {
	t.Helper()
	script := filepath.Join("..", "process", "testdata", "descendant.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("descendant script: %v", err)
	}
	cmd := &process.Command{
		Name:       name,
		Entrypoint: "/bin/bash",
		Args:       []string{script, gen, "stay-setsid"},
	}
	cmd.SetState(st)
	return cmd
}

func startOutsider(t *testing.T) int {
	t.Helper()
	outsider := exec.Command("/bin/sleep", "120")
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if outsider.Process != nil {
			_ = outsider.Process.Kill()
			_, _ = outsider.Process.Wait()
		}
	})
	return outsider.Process.Pid
}

func waitDirectAndDescendant(t *testing.T, st *process.State, name, gen string) (direct, descendant int) {
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

func waitDescendantReaped(t *testing.T, gen string, descendant int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pids := readPIDs(gen)
		if len(pids) > 1 && processAlive(descendant) {
			t.Fatalf("replacement descendant %d started while descendant %d was still running", pids[len(pids)-1], descendant)
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

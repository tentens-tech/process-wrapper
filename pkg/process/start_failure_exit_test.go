//go:build linux

package process

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestStartFailureDoesNotExitSupervisor is the spec for a start failure in
// Command.Run. Each scenario makes cmd.Start fail for one supervised command
// while a sibling command is running. The supervisor process must stay alive
// and the sibling must keep running.
//
// The scenarios are separate processes because a failing start currently
// terminates the caller. They cover the restart path and the first start,
// including replicas, a deleted or unusable working directory, and an
// entrypoint that still looks present to a stat or execute-bit check.
func TestStartFailureDoesNotExitSupervisor(t *testing.T) {
	scenarios := []string{
		"restart_deleted_entrypoint",
		"restart_entrypoint_replaced_with_directory",
		"restart_entrypoint_bad_interpreter",
		"restart_deleted_workdir",
		"restart_unsearchable_workdir",
		"restart_deleted_workdir_with_sysprocattr",
		"restart_replica_deleted_entrypoint",
		"initial_directory_entrypoint",
		"initial_missing_entrypoint",
		"initial_missing_workdir",
	}
	if os.Getenv("PW_START_FAILURE_CHILD") == "1" {
		runStartFailureScenario(t, os.Getenv("PW_SCENARIO"))
		return
	}

	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestStartFailureDoesNotExitSupervisor$", "-test.v")
			cmd.Env = append(os.Environ(),
				"PW_START_FAILURE_CHILD=1",
				"PW_SCENARIO="+scenario,
				"HEALTH_SOCKET=127.0.0.1:0",
				"METRICS_SERVER_ADDRESS=",
				"HANDLE_PREEMPTION=",
				"HANDLE_TERMINATION=",
			)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatalf("start supervisor test process: %v", err)
			}
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err != nil {
				pgid = cmd.Process.Pid
			}
			defer func() {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}()

			done := make(chan error, 1)
			go func() {
				done <- cmd.Wait()
			}()
			select {
			case waitErr := <-done:
				out := output.String()
				marker := "SUPERVISOR_SURVIVED " + scenario
				if waitErr != nil || !strings.Contains(out, marker) {
					t.Fatalf("supervisor did not survive %s (process error: %v)\n%s", scenario, waitErr, out)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("supervisor test process timed out in %s\n%s", scenario, output.String())
			}
		})
	}
}

func runStartFailureScenario(t *testing.T, scenario string) {
	t.Helper()
	os.Unsetenv("HANDLE_PREEMPTION")
	os.Unsetenv("HANDLE_TERMINATION")
	os.Unsetenv("METRICS_SERVER_ADDRESS")
	if os.Getenv("HEALTH_SOCKET") == "" {
		os.Setenv("HEALTH_SOCKET", "127.0.0.1:0")
	}

	base := t.TempDir()
	beatDir := filepath.Join(base, "beat")
	if err := os.MkdirAll(beatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(beatDir, "sibling.pid")
	beatPath := filepath.Join(beatDir, "sibling.beat")
	siblingPath := filepath.Join(base, "sibling.sh")
	writeExecutable(t, siblingPath, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$$\" > %q\nwhile true; do\n  printf 'alive\\n' > %q\n  sleep 0.2\ndone\n", pidPath, beatPath))

	ctx := context.Background()
	state := NewState(filepath.Join(base, "readiness"), false)
	var commands []Command
	var ready func() bool

	switch scenario {
	case "restart_deleted_entrypoint":
		victim := filepath.Join(base, "victim.sh")
		writeExecutable(t, victim, "#!/bin/sh\nrm -f -- \"$0\"\nexit 0\n")
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: victim},
		}
		ready = func() bool {
			_, err := os.Lstat(victim)
			return os.IsNotExist(err)
		}
	case "restart_entrypoint_replaced_with_directory":
		victim := filepath.Join(base, "victim.sh")
		writeExecutable(t, victim, "#!/bin/sh\ntarget=$0\nrm -f -- \"$target\"\nmkdir -- \"$target\"\nexit 0\n")
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: victim},
		}
		ready = func() bool {
			info, err := os.Lstat(victim)
			return err == nil && info.IsDir()
		}
	case "restart_entrypoint_bad_interpreter":
		victim := filepath.Join(base, "victim.sh")
		// The replacement is still a regular executable file, so a stat or
		// access(X_OK) of the entrypoint succeeds. The kernel rejects it
		// when the supervisor starts it again because the interpreter is gone.
		writeExecutable(t, victim, fmt.Sprintf("#!/bin/sh\ntmp=%q\ncat > \"$tmp\" << 'EOF'\n#!/no/such/interpreter\necho hi\nEOF\nchmod 0755 \"$tmp\"\nmv -f \"$tmp\" \"$0\"\nexit 0\n", victim+".new"))
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: victim},
		}
		ready = func() bool {
			body, err := os.ReadFile(victim)
			return err == nil && strings.Contains(string(body), "#!/no/such/interpreter")
		}
	case "restart_deleted_workdir":
		commands, ready = workdirRestartCommands(t, base, siblingPath, false)
	case "restart_unsearchable_workdir":
		work := filepath.Join(base, "work")
		if err := os.Mkdir(work, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(base, "victim.sh")
		writeExecutable(t, victim, "#!/bin/sh\ndir=$1\ncd / || exit 1\nchmod 000 \"$dir\" || exit 1\nexit 0\n")
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: victim, Args: []string{work}, Dir: work},
		}
		ready = func() bool {
			info, err := os.Stat(work)
			if err != nil {
				return false
			}
			return info.IsDir() && info.Mode().Perm()&0o111 == 0
		}
	case "restart_deleted_workdir_with_sysprocattr":
		commands, ready = workdirRestartCommands(t, base, siblingPath, true)
	case "restart_replica_deleted_entrypoint":
		victim := filepath.Join(base, "victim.sh")
		writeExecutable(t, victim, "#!/bin/sh\nrm -f -- \"$0\"\nexit 0\n")
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: victim, Replicas: 1},
		}
		ready = func() bool {
			_, err := os.Lstat(victim)
			return os.IsNotExist(err)
		}
	case "initial_directory_entrypoint":
		dirEntry := filepath.Join(base, "entrypoint-dir")
		if err := os.Mkdir(dirEntry, 0o755); err != nil {
			t.Fatal(err)
		}
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: dirEntry},
		}
		ready = func() bool { return true }
	case "initial_missing_entrypoint":
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: filepath.Join(base, "missing-entrypoint")},
		}
		ready = func() bool { return true }
	case "initial_missing_workdir":
		entrypoint := filepath.Join(base, "victim.sh")
		writeExecutable(t, entrypoint, "#!/bin/sh\nexit 0\n")
		commands = []Command{
			{Name: "sibling", Entrypoint: siblingPath},
			{Name: "victim", Entrypoint: entrypoint, Dir: filepath.Join(base, "missing-workdir")},
		}
		ready = func() bool { return true }
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	NewSchedule(ctx, commands, nil, "", state, &wg)

	if err := waitUntil(5*time.Second, func() bool {
		_, err := os.Stat(pidPath)
		return err == nil
	}); err != nil {
		t.Fatalf("sibling did not start: %v", err)
	}
	siblingPID, err := readPID(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitUntil(5*time.Second, ready); err != nil {
		t.Fatalf("sabotage did not land: %v", err)
	}
	beforeBeat, err := os.Stat(beatPath)
	if err != nil {
		t.Fatalf("sibling heartbeat missing: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(siblingPID) {
			t.Fatalf("sibling %d is not running", siblingPID)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !processAlive(siblingPID) {
		t.Fatalf("sibling %d is not running", siblingPID)
	}
	afterBeat, err := os.Stat(beatPath)
	if err != nil {
		t.Fatalf("sibling heartbeat missing: %v", err)
	}
	if !afterBeat.ModTime().After(beforeBeat.ModTime()) {
		t.Fatalf("sibling stopped updating its heartbeat")
	}
	fmt.Printf("SUPERVISOR_SURVIVED %s\n", scenario)
}

func workdirRestartCommands(t *testing.T, base, siblingPath string, withSysProcAttr bool) ([]Command, func() bool) {
	t.Helper()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "victim.sh")
	writeExecutable(t, victim, "#!/bin/sh\ndir=$1\ncd / || exit 1\nrm -rf -- \"$dir\"\nexit 0\n")
	command := Command{
		Name:       "victim",
		Entrypoint: victim,
		Args:       []string{work},
		Dir:        work,
	}
	if withSysProcAttr {
		// A non-nil SysProcAttr makes os.StartProcess skip its pre-start Stat of Dir
		// and report the missing directory from the chdir that happens at exec time.
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	ready := func() bool {
		_, err := os.Lstat(work)
		return os.IsNotExist(err)
	}
	return []Command{{Name: "sibling", Entrypoint: siblingPath}, command}, ready
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitUntil(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return fmt.Errorf("condition not met within %s", timeout)
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("pid %d from %s", pid, path)
	}
	return pid, nil
}

func processAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	text := string(data)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return false
	}
	state := text[end+2]
	return state != 'Z' && state != 'X'
}

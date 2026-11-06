package process

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	rootOnlyMarker     = "root-only-marker"
	supervisorEnvName  = "PROBE_SUPERVISOR_MARKER"
	supervisorEnvValue = "supervisor-env-marker"
	probeOnlyEnvName   = "PROBE_ONLY"
	probeOnlyEnvValue  = "probe-env-marker"
)

// These tests are the spec for exec readiness probes. They fail while a probe
// ignores the supervised command's credential, chroot, directory, environment,
// and lifetime context, and they pass once the probe uses those settings.

func TestExecReadinessProbeRunsAsSupervisedUser(t *testing.T) {
	worker := requireWorker(t)
	t.Setenv(supervisorEnvName, supervisorEnvValue)

	rootOnly := filepath.Join(t.TempDir(), "root-only")
	if err := os.WriteFile(rootOnly, []byte(rootOnlyMarker), 0o400); err != nil {
		t.Fatal(err)
	}

	workerDir := accessibleDir(t, worker)
	outDir := filepath.Join(workerDir, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(outDir, int(worker.uid), int(worker.gid)); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(workerDir, "probe.sh")

	victim := exec.Command("/bin/sleep", "120")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = victim.Process.Kill()
		_, _ = victim.Process.Wait()
	})

	// The sandboxed worker replaces the probe script. Paths are absolute so the
	// script still runs when the probe environment is only the probe map.
	script := fmt.Sprintf(`#!/bin/sh
/usr/bin/id -u > %s
/usr/bin/id -g > %s
/bin/readlink /proc/self/cwd > %s
/usr/bin/tr '\0' '\n' < /proc/self/environ > %s
if content=$(/bin/cat %s); then
  printf '%%s\n' "$content" > %s
  printf 'copied\n' > %s
else
  printf 'denied\n' > %s
fi
if /bin/kill -s TERM %d; then
  printf 'signaled\n' > %s
else
  printf 'denied\n' > %s
fi
printf 'done\n' > %s
exit 0
`,
		shQuote(filepath.Join(outDir, "uid")),
		shQuote(filepath.Join(outDir, "gid")),
		shQuote(filepath.Join(outDir, "cwd")),
		shQuote(filepath.Join(outDir, "env")),
		shQuote(rootOnly),
		shQuote(filepath.Join(outDir, "copied")),
		shQuote(filepath.Join(outDir, "copy-status")),
		shQuote(filepath.Join(outDir, "copy-status")),
		victim.Process.Pid,
		shQuote(filepath.Join(outDir, "kill-status")),
		shQuote(filepath.Join(outDir, "kill-status")),
		shQuote(filepath.Join(outDir, "done")),
	)
	rewriteAsWorker(t, worker, scriptPath, script)

	direct := exec.Command("/usr/bin/id", "-u")
	direct.SysProcAttr = worker.attr()
	directOut, err := direct.Output()
	if err != nil {
		t.Fatalf("os/exec credential control did not drop uid: %v", err)
	}
	if strings.TrimSpace(string(directOut)) != strconv.Itoa(int(worker.uid)) {
		t.Fatalf("os/exec credential control uid = %q, want %d", strings.TrimSpace(string(directOut)), worker.uid)
	}

	raw := fmt.Sprintf(`
- name: worker
  entrypoint: /bin/sleep
  args: ["120"]
  dir: %s
  sysProcAttr:
    credential:
      uid: %d
      gid: %d
      groups: [%d]
  readiness:
    period: "1h"
    delay: "1ms"
    failureThreshold: 100
    exec:
      command:
        entrypoint: %s
        envs: {}
        sysProcAttr:
          credential:
            uid: 1
            gid: 1
            groups: [1]
`, yamlScalar(workerDir), worker.uid, worker.gid, worker.gid, yamlScalar(scriptPath))

	cmd := loadCommand(t, raw)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("yaml config did not populate the supervised credential")
	}
	if cmd.SysProcAttr.Credential.Uid != worker.uid || cmd.SysProcAttr.Credential.Gid != worker.gid {
		t.Fatalf("yaml credential = %d:%d, want %d:%d", cmd.SysProcAttr.Credential.Uid, cmd.SysProcAttr.Credential.Gid, worker.uid, worker.gid)
	}
	assertJSONCredential(t, raw, worker)

	ctx := startHealthChecker(t, &cmd)
	defer ctx()

	done := filepath.Join(outDir, "done")
	if _, err := waitForFile(t, done, 5*time.Second); err != nil {
		t.Fatalf("exec probe did not finish: %v", err)
	}

	uid := strings.TrimSpace(mustRead(t, filepath.Join(outDir, "uid")))
	gid := strings.TrimSpace(mustRead(t, filepath.Join(outDir, "gid")))
	cwd := strings.TrimSpace(mustRead(t, filepath.Join(outDir, "cwd")))
	envDump := mustRead(t, filepath.Join(outDir, "env"))
	copyStatus := strings.TrimSpace(mustRead(t, filepath.Join(outDir, "copy-status")))
	killStatus := strings.TrimSpace(mustRead(t, filepath.Join(outDir, "kill-status")))

	if uid != strconv.Itoa(int(worker.uid)) {
		t.Errorf("exec probe uid = %s, want supervised uid %d", uid, worker.uid)
	}
	if gid != strconv.Itoa(int(worker.gid)) {
		t.Errorf("exec probe gid = %s, want supervised gid %d", gid, worker.gid)
	}
	wantCwd, err := filepath.EvalSymlinks(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		gotCwd = cwd
	}
	if gotCwd != wantCwd {
		t.Errorf("exec probe cwd = %s, want supervised dir %s", gotCwd, wantCwd)
	}
	if strings.Contains(envDump, supervisorEnvName) || strings.Contains(envDump, supervisorEnvValue) {
		t.Errorf("empty probe env inherited %s from the supervisor", supervisorEnvName)
	}
	if copyStatus != "denied" {
		t.Errorf("exec probe copy-status = %s, want denied", copyStatus)
	}
	if killStatus != "denied" {
		t.Errorf("exec probe kill-status = %s, want denied", killStatus)
	}
	if !processAlive(victim.Process.Pid) {
		t.Errorf("supervised co-process %d is not alive after the exec probe", victim.Process.Pid)
	}
}

func TestExecReadinessProbeEmptyEnvDoesNotInheritSupervisor(t *testing.T) {
	worker := requireWorker(t)
	t.Setenv(supervisorEnvName, supervisorEnvValue)

	for _, tc := range []struct {
		name string
		envs string
	}{
		{name: "nil", envs: ""},
		{name: "empty", envs: "        envs: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := accessibleDir(t, worker)
			outDir := filepath.Join(base, "out")
			if err := os.Mkdir(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(outDir, int(worker.uid), int(worker.gid)); err != nil {
				t.Fatal(err)
			}
			scriptPath := filepath.Join(base, "env.sh")
			script := fmt.Sprintf(`#!/bin/sh
/usr/bin/tr '\0' '\n' < /proc/self/environ > %s
printf 'done\n' > %s
exit 0
`, shQuote(filepath.Join(outDir, "env")), shQuote(filepath.Join(outDir, "done")))
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			raw := fmt.Sprintf(`
- name: worker
  entrypoint: /bin/sleep
  args: ["30"]
  sysProcAttr:
    credential:
      uid: %d
      gid: %d
      groups: [%d]
  readiness:
    period: "1h"
    delay: "1ms"
    failureThreshold: 100
    exec:
      command:
        entrypoint: %s
%s`, worker.uid, worker.gid, worker.gid, yamlScalar(scriptPath), tc.envs)
			cmd := loadCommand(t, raw)
			stop := startHealthChecker(t, &cmd)
			defer stop()
			if _, err := waitForFile(t, filepath.Join(outDir, "done"), 5*time.Second); err != nil {
				t.Fatalf("exec probe did not finish: %v", err)
			}
			envDump := mustRead(t, filepath.Join(outDir, "env"))
			if strings.Contains(envDump, supervisorEnvName) || strings.Contains(envDump, supervisorEnvValue) {
				t.Errorf("probe env inherited %s from the supervisor", supervisorEnvName)
			}
		})
	}
}

func TestExecReadinessProbeEnvMapExcludesSupervisor(t *testing.T) {
	worker := requireWorker(t)
	t.Setenv(supervisorEnvName, supervisorEnvValue)

	base := accessibleDir(t, worker)
	outDir := filepath.Join(base, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(outDir, int(worker.uid), int(worker.gid)); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(base, "env.sh")
	script := fmt.Sprintf(`#!/bin/sh
/usr/bin/tr '\0' '\n' < /proc/self/environ > %s
printf 'done\n' > %s
exit 0
`, shQuote(filepath.Join(outDir, "env")), shQuote(filepath.Join(outDir, "done")))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	raw := fmt.Sprintf(`
- name: worker
  entrypoint: /bin/sleep
  args: ["30"]
  sysProcAttr:
    credential:
      uid: %d
      gid: %d
      groups: [%d]
  readiness:
    period: "1h"
    delay: "1ms"
    failureThreshold: 100
    exec:
      command:
        entrypoint: %s
        envs:
          %s: %s
`, worker.uid, worker.gid, worker.gid, yamlScalar(scriptPath), probeOnlyEnvName, probeOnlyEnvValue)
	cmd := loadCommand(t, raw)
	stop := startHealthChecker(t, &cmd)
	defer stop()
	if _, err := waitForFile(t, filepath.Join(outDir, "done"), 5*time.Second); err != nil {
		t.Fatalf("exec probe did not finish: %v", err)
	}
	envDump := mustRead(t, filepath.Join(outDir, "env"))
	if !strings.Contains(envDump, probeOnlyEnvName+"="+probeOnlyEnvValue) {
		t.Errorf("probe env map entry %s missing from probe process", probeOnlyEnvName)
	}
	if strings.Contains(envDump, supervisorEnvName) || strings.Contains(envDump, supervisorEnvValue) {
		t.Errorf("probe env map was merged with %s from the supervisor", supervisorEnvName)
	}
}

func TestExecReadinessProbeUsesSupervisedChroot(t *testing.T) {
	worker := requireWorker(t)
	t.Setenv(supervisorEnvName, supervisorEnvValue)

	helper := buildStaticHelper(t)
	rootfs := t.TempDir()
	if err := os.Chmod(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"out", "work"} {
		if err := os.Mkdir(filepath.Join(rootfs, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(rootfs, "out"), 0o777); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(rootfs, "probe")
	if err := copyFile(helper, probePath, 0o755); err != nil {
		t.Fatal(err)
	}
	hostMarker := filepath.Join(t.TempDir(), "host-marker")
	if err := os.WriteFile(hostMarker, []byte("host-marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrongRoot := t.TempDir()
	result := filepath.Join(rootfs, "out", "result")

	control := exec.Command("/probe", "/out/result", hostMarker)
	control.Dir = "/work"
	control.Env = []string{}
	control.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     rootfs,
		Credential: &syscall.Credential{Uid: worker.uid, Gid: worker.gid, Groups: []uint32{worker.gid}},
	}
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("chroot credential control failed: %v %s", err, out)
	}
	controlBody := mustRead(t, result)
	if !strings.Contains(controlBody, "marker=hidden") || !strings.Contains(controlBody, "uid="+strconv.Itoa(int(worker.uid))) {
		t.Fatalf("chroot credential control produced unexpected result:\n%s", controlBody)
	}
	if err := os.Remove(result); err != nil {
		t.Fatal(err)
	}

	raw := fmt.Sprintf(`
- name: worker
  entrypoint: /bin/sleep
  args: ["30"]
  dir: /work
  sysProcAttr:
    chroot: %s
    credential:
      uid: %d
      gid: %d
      groups: [%d]
  readiness:
    period: "1h"
    delay: "1ms"
    failureThreshold: 100
    exec:
      command:
        entrypoint: /probe
        args: ["/out/result", %s]
        envs: {}
        dir: /tmp
        sysProcAttr:
          chroot: %s
          credential:
            uid: 1
            gid: 1
            groups: [1]
`, yamlScalar(rootfs), worker.uid, worker.gid, worker.gid, yamlScalar(hostMarker), yamlScalar(wrongRoot))
	cmd := loadCommand(t, raw)
	stop := startHealthChecker(t, &cmd)
	defer stop()

	body, err := waitForFile(t, result, 5*time.Second)
	if err != nil {
		t.Fatalf("exec probe did not run inside the supervised chroot %s: %v", rootfs, err)
	}
	text := string(body)
	if !strings.Contains(text, "uid="+strconv.Itoa(int(worker.uid))) {
		t.Errorf("chrooted probe uid mismatch:\n%s", text)
	}
	if !strings.Contains(text, "gid="+strconv.Itoa(int(worker.gid))) {
		t.Errorf("chrooted probe gid mismatch:\n%s", text)
	}
	if !strings.Contains(text, "cwd=/work") {
		t.Errorf("chrooted probe cwd mismatch:\n%s", text)
	}
	if !strings.Contains(text, "marker=hidden") {
		t.Errorf("chrooted probe can see host file %s:\n%s", hostMarker, text)
	}
	if !strings.Contains(text, "env=absent") {
		t.Errorf("chrooted probe inherited supervisor env:\n%s", text)
	}
}

func TestExecReadinessProbeStopsWhenCommandContextIsCanceled(t *testing.T) {
	requireRoot(t)
	outDir := t.TempDir()
	pidFile := filepath.Join(outDir, "pid")
	scriptPath := filepath.Join(outDir, "sleep.sh")
	script := fmt.Sprintf("#!/bin/sh\necho $$ > %s\nexec /bin/sleep 120\n", shQuote(pidFile))
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	raw := fmt.Sprintf(`
- name: worker
  entrypoint: /bin/sleep
  args: ["30"]
  readiness:
    period: "1h"
    delay: "1ms"
    failureThreshold: 100
    exec:
      command:
        entrypoint: %s
`, yamlScalar(scriptPath))
	cmd := loadCommand(t, raw)
	stop := startHealthChecker(t, &cmd)
	t.Cleanup(func() {
		stop()
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	data, err := waitForFile(t, pidFile, 5*time.Second)
	if err != nil {
		t.Fatalf("exec probe did not start: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !processAlive(pid) {
		t.Fatalf("probe pid %d was not alive before cancel", pid)
	}

	// Shutdown cancels the health-checker context (State.CancelRoutines).
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("exec probe pid %d still running 2s after the command context was canceled", pid)
}

type workerIDs struct {
	uid uint32
	gid uint32
}

func (w workerIDs) attr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: w.uid, Gid: w.gid, Groups: []uint32{w.gid}},
	}
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("exec probe tests require root, euid=%d", os.Geteuid())
	}
}

func requireWorker(t *testing.T) workerIDs {
	t.Helper()
	requireRoot(t)
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if uid == 0 {
		t.Fatal("nobody uid is 0")
	}
	return workerIDs{uid: uint32(uid), gid: uint32(gid)}
}

func rewriteAsWorker(t *testing.T, worker workerIDs, path, content string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `cat > "$1" && chmod 0755 "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	cmd.SysProcAttr = worker.attr()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker could not replace probe script: %v %s", err, out)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("stat type is not syscall.Stat_t")
	}
	if stat.Uid != worker.uid {
		t.Fatalf("probe script owner uid = %d, want worker uid %d", stat.Uid, worker.uid)
	}
}

func loadCommand(t *testing.T, raw string) Command {
	t.Helper()
	var commands []Command
	if err := yaml.Unmarshal([]byte(raw), &commands); err != nil {
		t.Fatalf("unmarshal config: %v\n%s", err, raw)
	}
	if len(commands) != 1 {
		t.Fatalf("command count = %d, want 1", len(commands))
	}
	if commands[0].Readiness == nil || commands[0].Readiness.Exec == nil {
		t.Fatal("config has no exec readiness probe")
	}
	return commands[0]
}

func assertJSONCredential(t *testing.T, yamlRaw string, worker workerIDs) {
	t.Helper()
	var fromYAML []Command
	if err := yaml.Unmarshal([]byte(yamlRaw), &fromYAML); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON []Command
	if err := json.Unmarshal(encoded, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if fromJSON[0].SysProcAttr == nil || fromJSON[0].SysProcAttr.Credential == nil {
		t.Fatal("json config did not populate the supervised credential")
	}
	if fromJSON[0].SysProcAttr.Credential.Uid != worker.uid {
		t.Fatalf("json credential uid = %d, want %d", fromJSON[0].SysProcAttr.Credential.Uid, worker.uid)
	}
}

func startHealthChecker(t *testing.T, cmd *Command) context.CancelFunc {
	t.Helper()
	cmd.s = &State{
		killedMap:        make(map[string]bool),
		statusMap:        make(map[string]bool),
		pidsMap:          make(map[string]int),
		floatingPid:      make(map[string][]time.Time),
		heartBeat:        make(map[string]time.Time),
		cancelRoutineMap: make(map[string]context.CancelFunc),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go cmd.HealthChecker(ctx, cancel, 1<<30-1, nil)
	return cancel
}

func accessibleDir(t *testing.T, worker workerIDs) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "exec-probe-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, int(worker.uid), int(worker.gid)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitForFile(t *testing.T, path string, timeout time.Duration) ([]byte, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(bytesTrimSpace(data)) > 0 {
			return data, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("file %s was empty", path)
	}
	return nil, lastErr
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func processAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	text := string(data)
	i := strings.LastIndex(text, ")")
	if i < 0 || i+2 >= len(text) {
		return true
	}
	return text[i+2] != 'Z'
}

func shQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func yamlScalar(value string) string {
	return strconv.Quote(value)
}

func buildStaticHelper(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "probe")
	cmd := exec.Command("go", "build", "-o", out, "testdata/probe_helper.go")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build probe helper: %v %s", err, output)
	}
	ldd := exec.Command("ldd", out)
	lddOut, _ := ldd.CombinedOutput()
	if !strings.Contains(string(lddOut), "not a dynamic executable") {
		t.Fatalf("probe helper is not static: %s", lddOut)
	}
	return out
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

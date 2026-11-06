package process

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Post-start command hooks and exec probes are specified by these tests.
//
// A supervised command can run as a non-root uid via SysProcAttr.Credential.
// After probes pass, ProbeRunner calls PostStartHookRunner, which calls
// Command.RunHook. RunHook applies only the hook command's own SysProcAttr and
// passes the supervised command's merged environment. The same gap exists on
// every PostStart entry point and on exec readiness probes.
//
// Attacker-controlled programs (a replaced hook entrypoint, a replaced script
// argument, a script that entrypoint executes, or a binary swapped after a
// path check and before exec) must not run as the supervisor uid. A hook that
// does run must see the hook's own environment and not the supervisor's.
// Refusal is a successful outcome for an attacker-controlled program.
//
// SQL hooks are not covered: SQLHook.RunHook opens a database connection and
// does not exec a program. Replica commands are not covered: NewSchedule does
// not copy PostStart onto replicas, so those commands never reach RunHook.

const (
	supervisorMarker = "supervisor-only-marker"
	commandMarker    = "command-env-marker"
	hookMarker       = "hook-local-marker"
)

func TestMain(m *testing.M) {
	switch {
	case os.Getenv("PW_SECCOMP_SUPERVISOR") == "1":
		seccompSupervisor()
		os.Exit(0)
	case os.Getenv("PW_TOCTOU_CHILD") == "1":
		os.Exit(runTOCTOUChild())
	default:
		os.Exit(m.Run())
	}
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("these tests must run as root (sudo go test ./pkg/process/ -count=1 -run 'TestPostStart|TestExecProbe')")
	}
}

func workerIDs(t *testing.T) (uint32, uint32) {
	t.Helper()
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Fatalf("lookup nobody: %v", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if uid == 0 || gid == 0 {
		t.Fatalf("nobody uid/gid = %d/%d", uid, gid)
	}
	return uint32(uid), uint32(gid)
}

func cred(uid, gid uint32) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
}

func newTestState() *State {
	return &State{
		killedMap:        make(map[string]bool),
		statusMap:        make(map[string]bool),
		pidsMap:          make(map[string]int),
		floatingPid:      make(map[string][]time.Time),
		heartBeat:        make(map[string]time.Time),
		cancelRoutineMap: make(map[string]context.CancelFunc),
		preemptedEvent:   make(chan struct{}),
	}
}

func markerScript(kind string) string {
	if strings.ContainsAny(kind, "\n'\"\\$`") {
		panic("kind must be a plain token")
	}
	const script = `#!/bin/sh
# payload-kind=KIND
out=$1
status_uid=
while IFS= read -r line; do
  case $line in
    Uid:*)
      for field in $line; do
        case $field in
          Uid:) ;;
          *) status_uid=$field; break ;;
        esac
      done
      ;;
  esac
done < /proc/self/status
id_uid=$(/usr/bin/id -u)
{
  printf 'kind=%s\n' 'KIND'
  printf 'uid=%s\n' "$status_uid"
  printf 'id_uid=%s\n' "$id_uid"
  printf 'supervisor=%s\n' "$PW_SUPERVISOR_SECRET"
  printf 'command=%s\n' "$PW_COMMAND_SECRET"
  printf 'hook=%s\n' "$HOOK_LOCAL"
} > "$out"
`
	return strings.ReplaceAll(script, "KIND", kind)
}

type hookResult struct {
	ran        bool
	kind       string
	uid        int
	idUID      int
	supervisor string
	command    string
	hook       string
	raw        string
}

func readHookResult(path string) hookResult {
	b, err := os.ReadFile(path)
	if err != nil {
		return hookResult{}
	}
	res := hookResult{ran: true, raw: string(b)}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "kind":
			res.kind = v
		case "uid":
			res.uid, _ = strconv.Atoi(v)
		case "id_uid":
			res.idUID, _ = strconv.Atoi(v)
		case "supervisor":
			res.supervisor = v
		case "command":
			res.command = v
		case "hook":
			res.hook = v
		}
	}
	return res
}

func runAs(t *testing.T, uid, gid uint32, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = cred(uid, gid)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s as uid %d: %v\n%s", name, uid, err, out)
	}
	return string(out)
}

func writeScript(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func prepareWorkspace(t *testing.T) (dir, out string) {
	t.Helper()
	dir = t.TempDir()
	// t.TempDir's parent is created mode 0700, so a dropped worker cannot
	// traverse it. The worker must be able to reach and replace files here.
	if err := os.Chmod(filepath.Dir(dir), 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(dir, "outcome")
	if err := os.WriteFile(out, nil, 0666); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies umask, so force a mode the worker and the supervisor can both write.
	if err := os.Chmod(out, 0666); err != nil {
		t.Fatal(err)
	}
	return dir, out
}

func supervisedCommand(name, out string, uid, gid uint32) *Command {
	return &Command{
		Name: name,
		Envs: map[string]string{
			"PW_COMMAND_SECRET": commandMarker,
		},
		SysProcAttr: cred(uid, gid),
		s:           newTestState(),
		PostStart: &Hook{
			Envs: map[string]string{"HOOK_LOCAL": hookMarker},
			Command: &Command{
				Entrypoint: filepath.Join(out, "unused"),
				Envs:       map[string]string{"HOOK_LOCAL": hookMarker},
			},
		},
	}
}

func armHook(cmd *Command, entry string, args ...string) {
	cmd.PostStart.Command.Entrypoint = entry
	cmd.PostStart.Command.Args = args
	cmd.PostStart.Command.Envs = map[string]string{"HOOK_LOCAL": hookMarker}
	cmd.PostStart.Envs = map[string]string{"HOOK_LOCAL": hookMarker}
}

func withSupervisorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PW_SUPERVISOR_SECRET", supervisorMarker)
}

func replaceAsWorker(t *testing.T, uid, gid uint32, dst, payload string) {
	t.Helper()
	runAs(t, uid, gid, "/bin/sh", "-c", `rm -f "$1" && cp "$2" "$1" && chmod 0755 "$1"`, "replace", dst, payload)
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("payload-kind=replaced")) {
		t.Fatalf("worker replace did not install payload into %s:\n%s", dst, got)
	}
}

func assertNoEscalation(t *testing.T, res hookResult, workerUID int) {
	t.Helper()
	if !res.ran {
		return
	}
	if res.kind != "replaced" {
		t.Fatalf("hook ran %q, want the worker-replaced program or no execution\n%s", res.kind, res.raw)
	}
	if res.uid != res.idUID {
		t.Fatalf("hook uid sources disagree: /proc/self/status %d and id %d\n%s", res.uid, res.idUID, res.raw)
	}
	if res.uid != workerUID {
		t.Fatalf("replaced hook ran as uid %d, supervised worker uid is %d\n%s", res.uid, workerUID, res.raw)
	}
	if res.supervisor != "" || res.command != "" {
		t.Fatalf("hook received supervisor environment supervisor=%q command=%q\n%s", res.supervisor, res.command, res.raw)
	}
}

func assertTrustedHookEnv(t *testing.T, res hookResult) {
	t.Helper()
	if !res.ran {
		t.Fatal("trusted post-start hook did not run")
	}
	if res.supervisor != "" || res.command != "" {
		t.Fatalf("hook received supervisor environment supervisor=%q command=%q\n%s", res.supervisor, res.command, res.raw)
	}
	if res.hook != hookMarker {
		t.Fatalf("hook environment = %q, want %q from the hook command\n%s", res.hook, hookMarker, res.raw)
	}
}

func mergedEnv(t *testing.T, cmd *Command) map[string]string {
	t.Helper()
	return cmd.MergeEnvs()
}

func TestPostStartHookAfterSuccessfulProbeDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	hookPath := filepath.Join(dir, "hook.sh")
	payload := filepath.Join(dir, "payload.sh")
	ready := filepath.Join(dir, "ready")
	writeScript(t, hookPath, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	replaceAsWorker(t, uid, gid, hookPath, payload)

	cmd := supervisedCommand("probe-pass", out, uid, gid)
	armHook(cmd, hookPath, out)
	cmd.Readiness = &Probe{
		Delay:            Duration{Duration: time.Millisecond},
		Period:           Duration{Duration: 20 * time.Millisecond},
		FailureThreshold: 20,
		File:             &FileProbe{Path: ready},
	}
	if err := os.WriteFile(ready, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	pid := startSleeper(t, uid, gid)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envs := mergedEnv(t, cmd)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd.ProbeRunner(ctx, cancel, log.WithField("name", cmd.Name), pid, envs)
	}()
	cancelWhenHookFinishes(out, cancel)
	waitDone(t, done)
	assertNoEscalation(t, readHookResult(out), int(uid))
}

func TestPostStartHookWithoutReadinessDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	hookPath := filepath.Join(dir, "hook.sh")
	payload := filepath.Join(dir, "payload.sh")
	writeScript(t, hookPath, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	replaceAsWorker(t, uid, gid, hookPath, payload)

	cmd := supervisedCommand("no-readiness", out, uid, gid)
	armHook(cmd, hookPath, out)
	cmd.HealthChecker(context.Background(), func() {}, 0, mergedEnv(t, cmd))
	assertNoEscalation(t, readHookResult(out), int(uid))
}

func TestPostStartHookWhenProbesUndefinedDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	hookPath := filepath.Join(dir, "hook.sh")
	payload := filepath.Join(dir, "payload.sh")
	writeScript(t, hookPath, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	replaceAsWorker(t, uid, gid, hookPath, payload)

	cmd := supervisedCommand("probes-undefined", out, uid, gid)
	armHook(cmd, hookPath, out)
	cmd.Readiness = &Probe{
		Delay:            Duration{Duration: time.Millisecond},
		Period:           Duration{Duration: time.Millisecond},
		FailureThreshold: 5,
	}
	cmd.HealthChecker(context.Background(), func() {}, 0, mergedEnv(t, cmd))
	assertNoEscalation(t, readHookResult(out), int(uid))
}

func TestPostStartHookScriptArgumentDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	script := filepath.Join(dir, "script.sh")
	payload := filepath.Join(dir, "payload.sh")
	writeScript(t, script, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	if workerCanWrite(t, uid, gid, "/bin/sh") {
		t.Fatal("worker can write /bin/sh; this case needs an immutable interpreter")
	}
	replaceAsWorker(t, uid, gid, script, payload)

	cmd := supervisedCommand("script-arg", out, uid, gid)
	armHook(cmd, "/bin/sh", script, out)
	cmd.HealthChecker(context.Background(), func() {}, 0, mergedEnv(t, cmd))
	assertNoEscalation(t, readHookResult(out), int(uid))
}

func TestPostStartHookCalledScriptDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	locked := t.TempDir()
	if err := os.Chmod(filepath.Dir(locked), 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "helper.sh")
	payload := filepath.Join(dir, "payload.sh")
	entry := filepath.Join(locked, "entry.sh")
	writeScript(t, helper, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	writeScript(t, entry, "#!/bin/sh\nexec \"$1\" \"$2\"\n", 0755)
	if err := os.Chown(entry, 0, 0); err != nil {
		t.Fatal(err)
	}
	if workerCanWrite(t, uid, gid, entry) {
		t.Fatal("worker can write the immutable entrypoint")
	}
	replaceAsWorker(t, uid, gid, helper, payload)

	cmd := supervisedCommand("called-script", out, uid, gid)
	armHook(cmd, entry, helper, out)
	cmd.HealthChecker(context.Background(), func() {}, 0, mergedEnv(t, cmd))
	assertNoEscalation(t, readHookResult(out), int(uid))
}

func TestPostStartHookEnvironmentExcludesSupervisorEnvironment(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	locked := t.TempDir()
	if err := os.Chmod(filepath.Dir(locked), 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0755); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := os.Chmod(outDir, 0711); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(outDir, "outcome")
	if err := os.WriteFile(out, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0666); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(locked, "trusted.sh")
	writeScript(t, hookPath, markerScript("trusted"), 0755)
	if err := os.Chown(hookPath, 0, 0); err != nil {
		t.Fatal(err)
	}
	if workerCanWrite(t, uid, gid, hookPath) {
		t.Fatal("worker can write the trusted hook")
	}

	cmd := supervisedCommand("trusted-env", out, uid, gid)
	armHook(cmd, hookPath, out)
	cmd.HealthChecker(context.Background(), func() {}, 0, mergedEnv(t, cmd))
	assertTrustedHookEnv(t, readHookResult(out))
}

func TestExecProbeDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	probePath := filepath.Join(dir, "probe.sh")
	payload := filepath.Join(dir, "payload.sh")
	writeScript(t, probePath, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	replaceAsWorker(t, uid, gid, probePath, payload)

	cmd := supervisedCommand("exec-probe", out, uid, gid)
	cmd.PostStart = nil
	cmd.Readiness = &Probe{
		Delay:            Duration{Duration: time.Millisecond},
		Period:           Duration{Duration: 20 * time.Millisecond},
		FailureThreshold: 5,
		Exec: &ExecProbe{Command: Command{
			Entrypoint: probePath,
			Args:       []string{out},
		}},
	}
	pid := startSleeper(t, uid, gid)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envs := mergedEnv(t, cmd)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd.ProbeRunner(ctx, cancel, log.WithField("name", cmd.Name), pid, envs)
	}()
	cancelWhenHookFinishes(out, cancel)
	waitDone(t, done)
	assertNoEscalation(t, readHookResult(out), int(uid))
}

func TestPostStartSupervisedCommandReplacesHookBeforeProbe(t *testing.T) {
	requireRoot(t)
	withSupervisorEnv(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	hookPath := filepath.Join(dir, "hook.sh")
	payload := filepath.Join(dir, "payload.sh")
	ready := filepath.Join(dir, "ready")
	workerOut := filepath.Join(dir, "worker")
	workerScript := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(workerOut, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workerOut, 0666); err != nil {
		t.Fatal(err)
	}
	writeScript(t, hookPath, markerScript("benign"), 0755)
	writeScript(t, payload, markerScript("replaced"), 0644)
	writeScript(t, workerScript, `#!/bin/sh
printf 'uid=%s\n' "$(/usr/bin/id -u)" > "$1"
rm -f "$2"
cp "$3" "$2"
chmod 0755 "$2"
: > "$4"
exec /bin/sleep 30
`, 0755)

	cmd := supervisedCommand("full-run", out, uid, gid)
	cmd.Entrypoint = workerScript
	cmd.Args = []string{workerOut, hookPath, payload, ready}
	armHook(cmd, hookPath, out)
	cmd.Readiness = &Probe{
		Delay:            Duration{Duration: 200 * time.Millisecond},
		Period:           Duration{Duration: 20 * time.Millisecond},
		FailureThreshold: 40,
		File:             &FileProbe{Path: ready},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go cmd.Run(ctx, &wg)

	deadline := time.Now().Add(8 * time.Second)
	var readyAt time.Time
	for time.Now().Before(deadline) {
		if fileNonEmpty(out) {
			break
		}
		if readyAt.IsZero() && fileExists(ready) {
			readyAt = time.Now()
		}
		if !readyAt.IsZero() && time.Since(readyAt) > time.Second {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cmd.s.SetShutdown()
	cancel()
	waitGroup(t, &wg, 5*time.Second)

	workerBody, err := os.ReadFile(workerOut)
	if err != nil {
		t.Fatalf("worker did not record its uid: %v", err)
	}
	workerUID := -1
	for _, line := range strings.Split(string(workerBody), "\n") {
		if strings.HasPrefix(line, "uid=") {
			workerUID, _ = strconv.Atoi(strings.TrimPrefix(line, "uid="))
		}
	}
	if workerUID != int(uid) {
		t.Fatalf("supervised command ran as uid %d, want %d (credential was not applied)", workerUID, uid)
	}
	hookBody, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(hookBody, []byte("payload-kind=replaced")) {
		t.Fatalf("worker did not replace the hook before probes:\n%s", hookBody)
	}
	res := readHookResult(out)
	if !res.ran && !fileNonEmpty(ready) {
		t.Fatal("readiness file was not created and the hook did not run")
	}
	assertNoEscalation(t, res, int(uid))
}

// TestPostStartHookReplacedBetweenCheckAndExecDoesNotEscalate swaps the hook
// directory entry after any metadata lookup of the hook path and before
// execve. A fix that checks the path and later execs that same path still
// runs the replaced program. Execing the inode that was checked does not.
func TestPostStartHookReplacedBetweenCheckAndExecDoesNotEscalate(t *testing.T) {
	requireRoot(t)
	uid, gid := workerIDs(t)
	dir, out := prepareWorkspace(t)
	hookPath := filepath.Join(dir, "hook.sh")
	payload := filepath.Join(dir, "payload.sh")
	writeScript(t, hookPath, markerScript("safe"), 0755)
	if err := os.Chown(hookPath, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hookPath, 0755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, payload, markerScript("replaced"), 0644)
	runAs(t, uid, gid, "/bin/sh", "-c", `touch "$1/worker-can-write"`, "touch", dir)
	if workerCanWrite(t, uid, gid, hookPath) {
		t.Fatal("hook inode is writable by the worker; the check must be able to observe a root-owned mode 0755 file")
	}

	stdout, stderr, err := runTOCTOUProcess(t, hookPath, out, payload, uid, gid)
	if err != nil {
		t.Fatalf("toctou child: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	t.Logf("toctou supervisor:\n%s", stderr)
	res := readHookResult(out)
	if res.ran && res.kind == "safe" {
		return
	}
	assertNoEscalation(t, res, int(uid))
}

func workerCanWrite(t *testing.T, uid, gid uint32, path string) bool {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `test -w "$1"`, "writable", path)
	cmd.SysProcAttr = cred(uid, gid)
	err := cmd.Run()
	return err == nil
}

func startSleeper(t *testing.T, uid, gid uint32) int {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = cred(uid, gid)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper as uid %d: %v", uid, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileNonEmpty(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

func cancelWhenHookFinishes(path string, cancel context.CancelFunc) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fileNonEmpty(path) {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("probe runner did not return")
	}
}

func waitGroup(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal("command did not return")
	}
}

func runTOCTOUProcess(t *testing.T, hook, out, payload string, uid, gid uint32) (string, string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	env := make([]string, 0, len(os.Environ())+8)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PW_TOCTOU_CHILD=") || strings.HasPrefix(e, "PW_SECCOMP_SUPERVISOR=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = append(env,
		"PW_TOCTOU_CHILD=1",
		"PW_HOOK_PATH="+hook,
		"PW_RESULT_PATH="+out,
		"PW_PWNED_FILE="+payload,
		"PW_WORKER_UID="+strconv.FormatUint(uint64(uid), 10),
		"PW_WORKER_GID="+strconv.FormatUint(uint64(gid), 10),
		"PW_SUPERVISOR_SECRET="+supervisorMarker,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return stdout.String(), stderr.String(), err
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return stdout.String(), stderr.String(), fmt.Errorf("timed out")
	}
}

func runTOCTOUChild() int {
	runtime.LockOSThread()
	hook := os.Getenv("PW_HOOK_PATH")
	out := os.Getenv("PW_RESULT_PATH")
	payload := os.Getenv("PW_PWNED_FILE")
	uid64, _ := strconv.ParseUint(os.Getenv("PW_WORKER_UID"), 10, 32)
	gid64, _ := strconv.ParseUint(os.Getenv("PW_WORKER_GID"), 10, 32)
	uid, gid := uint32(uid64), uint32(gid64)
	if hook == "" || out == "" || payload == "" || uid == 0 {
		fmt.Fprintln(os.Stderr, "toctou child missing environment")
		return 2
	}
	sup, err := startSeccompSupervisor(hook, payload, uid, gid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "supervisor: %v\n", err)
		return 2
	}
	defer func() { _ = sup.Process.Kill() }()
	sleeper := exec.Command("/bin/sleep", "30")
	sleeper.SysProcAttr = cred(uid, gid)
	if err := sleeper.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "sleeper: %v\n", err)
		return 2
	}
	defer func() { _ = sleeper.Process.Kill() }()
	if err := installHookSeccomp(sup.notify); err != nil {
		fmt.Fprintf(os.Stderr, "seccomp: %v\n", err)
		return 2
	}
	_ = os.Setenv("PW_SUPERVISOR_SECRET", supervisorMarker)
	cmd := supervisedCommand("toctou", out, uid, gid)
	armHook(cmd, hook, out)
	ready := filepath.Join(filepath.Dir(hook), "ready")
	if err := os.WriteFile(ready, []byte("ok"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "ready: %v\n", err)
		return 2
	}
	cmd.Readiness = &Probe{
		Delay:            Duration{Duration: time.Millisecond},
		Period:           Duration{Duration: 20 * time.Millisecond},
		FailureThreshold: 5,
		File:             &FileProbe{Path: ready},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelWhenHookFinishes(out, cancel)
	// Stay on the thread that installed the filter so the hook's exec is traced.
	cmd.ProbeRunner(ctx, cancel, log.WithField("name", cmd.Name), sleeper.Process.Pid, cmd.MergeEnvs())
	if b, err := os.ReadFile(out); err == nil {
		fmt.Fprintf(os.Stdout, "outcome:\n%s\n", b)
	} else {
		fmt.Fprintln(os.Stdout, "outcome: <none>")
	}
	return 0
}

type supervisorProc struct {
	Process *os.Process
	notify  *os.File
}

func startSeccompSupervisor(hook, payload string, uid, gid uint32) (*supervisorProc, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "seccomp-parent")
	child := os.NewFile(uintptr(fds[1]), "seccomp-child")
	env := make([]string, 0, len(os.Environ())+6)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PW_TOCTOU_CHILD=") || strings.HasPrefix(e, "PW_SECCOMP_SUPERVISOR=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"PW_SECCOMP_SUPERVISOR=1",
		"PW_HOOK_PATH="+hook,
		"PW_PWNED_FILE="+payload,
		"PW_WORKER_UID="+strconv.FormatUint(uint64(uid), 10),
		"PW_WORKER_GID="+strconv.FormatUint(uint64(gid), 10),
	)
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		parent.Close()
		child.Close()
		return nil, err
	}
	child.Close()
	return &supervisorProc{Process: cmd.Process, notify: parent}, nil
}

func installHookSeccomp(notifySock *os.File) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	var sizes struct {
		Notif uint16
		Resp  uint16
		Data  uint16
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_NOTIF_SIZES, 0, uintptr(unsafe.Pointer(&sizes))); errno != 0 {
		return fmt.Errorf("notif sizes: %v", errno)
	}
	if int(sizes.Notif) != int(unsafe.Sizeof(seccompNotif{})) || int(sizes.Resp) != int(unsafe.Sizeof(seccompNotifResp{})) {
		return fmt.Errorf("seccomp struct size notif=%d/%d resp=%d/%d", sizes.Notif, unsafe.Sizeof(seccompNotif{}), sizes.Resp, unsafe.Sizeof(seccompNotifResp{}))
	}
	filter := hookSeccompFilter()
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	fd, _, errno := unix.RawSyscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), uintptr(unix.SECCOMP_FILTER_FLAG_NEW_LISTENER), uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("seccomp filter: %v", errno)
	}
	listener := int(fd)
	unix.CloseOnExec(listener)
	rights := unix.UnixRights(listener)
	if err := unix.Sendmsg(int(notifySock.Fd()), []byte{0}, rights, nil, 0); err != nil {
		return fmt.Errorf("send listener: %w", err)
	}
	unix.Close(listener)
	return nil
}

func hookSeccompFilter() []unix.SockFilter {
	notify := []uint32{
		unix.SYS_EXECVE,
		unix.SYS_EXECVEAT,
		unix.SYS_OPENAT,
		unix.SYS_NEWFSTATAT,
		unix.SYS_STATX,
		unix.SYS_FACCESSAT,
		unix.SYS_FACCESSAT2,
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 0, K: unix.AUDIT_ARCH_X86_64},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	for i, nr := range notify {
		filter = append(filter, unix.SockFilter{
			Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K,
			Jt:   uint8(len(notify) - i),
			Jf:   0,
			K:    nr,
		})
	}
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF},
	)
	return filter
}

type seccompData struct {
	Nr                 int32
	Arch               uint32
	InstructionPointer uint64
	Args               [6]uint64
}

type seccompNotif struct {
	ID    uint64
	Pid   uint32
	Flags uint32
	Data  seccompData
}

type seccompNotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

func seccompSupervisor() {
	hook := os.Getenv("PW_HOOK_PATH")
	payload := os.Getenv("PW_PWNED_FILE")
	uid64, _ := strconv.ParseUint(os.Getenv("PW_WORKER_UID"), 10, 32)
	gid64, _ := strconv.ParseUint(os.Getenv("PW_WORKER_GID"), 10, 32)
	sock := os.NewFile(3, "seccomp-sock")
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4*8))
	_, oobn, _, _, err := unix.Recvmsg(int(sock.Fd()), buf, oob, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recv listener: %v\n", err)
		os.Exit(1)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		fmt.Fprintf(os.Stderr, "parse rights: %v\n", err)
		os.Exit(1)
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		fmt.Fprintf(os.Stderr, "rights: %v\n", err)
		os.Exit(1)
	}
	listener := fds[0]
	body, err := os.ReadFile(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "payload: %v\n", err)
		os.Exit(1)
	}
	checked := false
	swapped := false
	swap := func() {
		if swapped {
			return
		}
		tmp := hook + ".next"
		if err := os.WriteFile(tmp, body, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "swap write: %v\n", err)
			return
		}
		if err := os.Chown(tmp, int(uid64), int(gid64)); err != nil {
			fmt.Fprintf(os.Stderr, "swap chown: %v\n", err)
			return
		}
		if err := os.Rename(tmp, hook); err != nil {
			fmt.Fprintf(os.Stderr, "swap rename: %v\n", err)
			return
		}
		swapped = true
		fmt.Fprintf(os.Stderr, "swapped %s after check=%v\n", hook, checked)
	}
	for {
		var req seccompNotif
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(listener), uintptr(unix.SECCOMP_IOCTL_NOTIF_RECV), uintptr(unsafe.Pointer(&req)))
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			fmt.Fprintf(os.Stderr, "notif recv: %v\n", errno)
			return
		}
		path := ""
		isExec := false
		switch req.Data.Nr {
		case unix.SYS_EXECVE:
			path = readRemoteString(int(req.Pid), req.Data.Args[0])
			isExec = true
		case unix.SYS_EXECVEAT:
			if req.Data.Args[4]&unix.AT_EMPTY_PATH == 0 {
				path = readRemoteString(int(req.Pid), req.Data.Args[1])
				isExec = true
			}
		case unix.SYS_OPENAT, unix.SYS_NEWFSTATAT, unix.SYS_STATX, unix.SYS_FACCESSAT, unix.SYS_FACCESSAT2:
			path = readRemoteString(int(req.Pid), req.Data.Args[1])
		}
		if path == hook {
			fmt.Fprintf(os.Stderr, "hook syscall nr=%d pid=%d exec=%v checked=%v\n", req.Data.Nr, req.Pid, isExec, checked)
			if !isExec && !checked {
				checked = true
				seccompContinue(listener, req.ID)
				swap()
				continue
			}
			swap()
		}
		seccompContinue(listener, req.ID)
	}
}

func seccompContinue(listener int, id uint64) {
	resp := seccompNotifResp{ID: id, Flags: unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(listener), uintptr(unix.SECCOMP_IOCTL_NOTIF_SEND), uintptr(unsafe.Pointer(&resp)))
	if errno != 0 && errno != unix.ENOENT {
		fmt.Fprintf(os.Stderr, "notif send: %v\n", errno)
	}
}

func readRemoteString(pid int, addr uint64) string {
	if addr == 0 {
		return ""
	}
	buf := make([]byte, 4096)
	n, err := unix.ProcessVMReadv(pid, []unix.Iovec{{
		Base: &buf[0],
		Len:  uint64(len(buf)),
	}}, []unix.RemoteIovec{{
		Base: uintptr(addr),
		Len:  len(buf),
	}}, 0)
	if err != nil || n == 0 {
		return ""
	}
	if n > len(buf) {
		n = len(buf)
	}
	if i := bytes.IndexByte(buf[:n], 0); i >= 0 {
		return string(buf[:i])
	}
	return string(buf[:n])
}

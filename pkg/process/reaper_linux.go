//go:build linux

package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Every supervised command is started under a reaper: this binary re-executed
// with reaperEnv set. The reaper marks itself as a child subreaper, so every
// orphaned descendant of the command (background jobs, double forks, setsid
// daemons) is reparented to it instead of escaping the supervisor. When the
// command exits, or the supervisor asks for a kill, the reaper kills and reaps
// all of them before it reports the command's exit status and exits.
//
// fd 3 (control) carries the reaperSpec from the supervisor; any further byte
// or EOF on it is a kill request. fd 4 (status) carries "pid N" once the
// command runs and "exit N" (a raw wait status) once no descendant is left.
const (
	reaperEnv   = "PROCESS_WRAPPER_REAPER"
	reaperArgv0 = "process-wrapper-reaper"
	reaperExe   = "/proc/self/exe"
	reaperCtlFd = 3
	reaperStsFd = 4
)

type reaperSpec struct {
	Path        string               `json:"path"`
	Args        []string             `json:"args"`
	Dir         string               `json:"dir"`
	SysProcAttr *syscall.SysProcAttr `json:"sysProcAttr"`
}

func init() {
	if os.Getenv(reaperEnv) != "1" {
		return
	}
	os.Exit(runReaper())
}

func runReaper() int {
	syscall.CloseOnExec(reaperCtlFd)
	syscall.CloseOnExec(reaperStsFd)
	ctl := os.NewFile(reaperCtlFd, "reaper-control")
	status := os.NewFile(reaperStsFd, "reaper-status")
	_ = os.Unsetenv(reaperEnv)

	// The supervisor signals the command pid directly. Swallow signals aimed
	// at the reaper (terminal and process-group signals) so it outlives the
	// command; Go resets caught signals to default in the command.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT,
		syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGPIPE,
		syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)

	ctlReader := bufio.NewReader(ctl)
	line, err := ctlReader.ReadBytes('\n')
	if err != nil {
		return 1
	}
	var spec reaperSpec
	if err = json.Unmarshal(line, &spec); err != nil {
		reportStatus(status, "error", err.Error())
		return 1
	}
	if err = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		reportStatus(status, "error", fmt.Sprintf("set child subreaper: %v", err))
		return 1
	}

	cmd := &exec.Cmd{
		Path:        spec.Path,
		Args:        spec.Args,
		Dir:         spec.Dir,
		Env:         os.Environ(),
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		SysProcAttr: spec.SysProcAttr,
	}
	if err = cmd.Start(); err != nil {
		reportStatus(status, "error", err.Error())
		return 1
	}
	reportStatus(status, "pid", strconv.Itoa(cmd.Process.Pid))

	go func() {
		_, _ = ctlReader.ReadByte()
		_ = cmd.Process.Kill()
	}()

	_ = cmd.Wait()
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	reapDescendants()
	reportStatus(status, "exit", strconv.Itoa(int(ws)))

	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func reportStatus(w io.Writer, kind, value string) {
	_, _ = fmt.Fprintf(w, "%s %s\n", kind, strings.ReplaceAll(value, "\n", " "))
}

// reapDescendants kills and reaps every remaining child of the reaper until it
// has none. As a subreaper, every live descendant has a chain of live parents
// ending at the reaper, so no children means no descendants.
func reapDescendants() {
	self := os.Getpid()
	for {
		children, err := childrenOf(self)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: cannot list children: %v\n", reaperArgv0, err)
		}
		for _, pid := range children {
			// An unreaped child's pid cannot be reused, and only this loop reaps.
			_ = unix.Kill(pid, unix.SIGKILL)
		}
		var ws unix.WaitStatus
		_, err = unix.Wait4(-1, &ws, 0, nil)
		switch err {
		case nil, unix.EINTR:
		case unix.ECHILD:
			return
		default:
			fmt.Fprintf(os.Stderr, "%s: wait: %v\n", reaperArgv0, err)
			return
		}
	}
}

func childrenOf(ppid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var children []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if parent, _, ok := procStat(pid); ok && parent == ppid {
			children = append(children, pid)
		}
	}
	return children, nil
}

// procStat returns the parent pid and state of pid from /proc/<pid>/stat.
func procStat(pid int) (ppid int, state byte, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	// Fields after the parenthesised comm: state, ppid, ...
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 2 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return ppid, fields[0][0], true
}

// reapers holds the pids of running reapers. The supervisor is a child
// subreaper too, so if a reaper is killed before it reaps, the descendants it
// held become children of the supervisor rather than of init.
var reapers = struct {
	sync.Mutex
	pids        map[int]struct{}
	subreaperOn bool
}{pids: map[int]struct{}{}}

func registerReaper(start func() error, pid func() int) error {
	reapers.Lock()
	defer reapers.Unlock()
	if !reapers.subreaperOn {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("set child subreaper: %w", err)
		}
		reapers.subreaperOn = true
	}
	if err := start(); err != nil {
		return err
	}
	reapers.pids[pid()] = struct{}{}
	return nil
}

func unregisterReaper(pid int) {
	reapers.Lock()
	delete(reapers.pids, pid)
	reapers.Unlock()
}

// killEscapedDescendants kills every live child of the supervisor that is not
// a running reaper, until none is left. Those can only be descendants of a
// reaper that was killed before it could reap them. Reapers stay locked so no
// new one starts; the caller exits afterwards.
func killEscapedDescendants() {
	reapers.Lock()
	self := os.Getpid()
	for {
		children, err := childrenOf(self)
		if err != nil {
			log.Errorf("Cannot list supervisor children, %v", err)
			return
		}
		alive := 0
		for _, pid := range children {
			if _, ok := reapers.pids[pid]; ok {
				continue
			}
			if killChildOf(self, pid) {
				alive++
			}
		}
		if alive == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killChildOf sends SIGKILL to pid if it is still a live child of parent. The
// pidfd pins the process, so a pid reused after another reaper's Wait is
// never signalled.
func killChildOf(parent, pid int) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err != unix.ENOSYS {
			return false
		}
		fd = -1
	}
	if fd >= 0 {
		defer unix.Close(fd)
	}
	ppid, state, ok := procStat(pid)
	if !ok || ppid != parent || state == 'Z' || state == 'X' {
		return false
	}
	if fd >= 0 {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	} else {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
	return true
}

// supervisedCmd runs a command under a reaper. Wait returns only after the
// command and every process it left behind are gone.
type supervisedCmd struct {
	Env         []string
	Dir         string
	SysProcAttr *syscall.SysProcAttr
	Stdout      io.Writer
	Stderr      io.Writer

	ctx      context.Context
	path     string
	args     []string
	lookErr  error
	shim     *exec.Cmd
	ctl      *os.File
	status   *os.File
	statusRd *bufio.Reader
	stopCtx  func() bool
	killOnce sync.Once
	pid      int
	ws       syscall.WaitStatus
	done     chan struct{}
}

func newSupervisedCmd(ctx context.Context, entrypoint string, args ...string) *supervisedCmd {
	resolved := exec.Command(entrypoint, args...) // #nosec G204
	shim := exec.Command(reaperExe)
	shim.Args = []string{reaperArgv0}
	return &supervisedCmd{
		ctx:     ctx,
		path:    resolved.Path,
		args:    resolved.Args,
		lookErr: resolved.Err,
		shim:    shim,
		done:    make(chan struct{}),
	}
}

func (s *supervisedCmd) StdoutPipe() (io.ReadCloser, error) { return s.shim.StdoutPipe() }
func (s *supervisedCmd) StderrPipe() (io.ReadCloser, error) { return s.shim.StderrPipe() }

func (s *supervisedCmd) Start() error {
	if s.lookErr != nil {
		return s.lookErr
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	spec, err := json.Marshal(reaperSpec{Path: s.path, Args: s.args, Dir: s.Dir, SysProcAttr: s.SysProcAttr})
	if err != nil {
		return err
	}
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		return err
	}
	stsR, stsW, err := os.Pipe()
	if err != nil {
		_ = ctlR.Close()
		_ = ctlW.Close()
		return err
	}
	env := s.Env
	if env == nil {
		env = os.Environ()
	}
	s.shim.Env = append(append([]string{}, env...), reaperEnv+"=1")
	s.shim.ExtraFiles = []*os.File{ctlR, stsW}
	if s.Stdout != nil {
		s.shim.Stdout = s.Stdout
	}
	if s.Stderr != nil {
		s.shim.Stderr = s.Stderr
	}
	err = registerReaper(s.shim.Start, func() int { return s.shim.Process.Pid })
	_ = ctlR.Close()
	_ = stsW.Close()
	if err != nil {
		_ = ctlW.Close()
		_ = stsR.Close()
		return err
	}
	s.ctl, s.status, s.statusRd = ctlW, stsR, bufio.NewReader(stsR)
	s.stopCtx = context.AfterFunc(s.ctx, s.Kill)

	if _, err = s.ctl.Write(append(spec, '\n')); err != nil {
		return s.abortStart(fmt.Errorf("send command to reaper: %w", err))
	}
	kind, value, err := s.readStatus()
	if err != nil {
		return s.abortStart(fmt.Errorf("reaper exited before starting the command: %w", err))
	}
	switch kind {
	case "pid":
		if s.pid, err = strconv.Atoi(value); err != nil {
			return s.abortStart(fmt.Errorf("reaper reported bad pid %q", value))
		}
		return nil
	case "error":
		return s.abortStart(fmt.Errorf("%s", value))
	default:
		return s.abortStart(fmt.Errorf("unexpected reaper status %q", kind))
	}
}

func (s *supervisedCmd) abortStart(err error) error {
	s.Kill()
	_, _ = io.Copy(io.Discard, s.statusRd)
	_ = s.shim.Wait()
	unregisterReaper(s.shim.Process.Pid)
	s.stopCtx()
	_ = s.status.Close()
	close(s.done)
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func (s *supervisedCmd) readStatus() (string, string, error) {
	line, err := s.statusRd.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	kind, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	return kind, value, nil
}

// Pid is the pid of the supervised command itself, not of its reaper.
func (s *supervisedCmd) Pid() int { return s.pid }

// Kill asks the reaper to kill the command and every descendant.
func (s *supervisedCmd) Kill() {
	s.killOnce.Do(func() {
		if s.ctl != nil {
			_ = s.ctl.Close()
		}
	})
}

// Done is closed once Wait has returned.
func (s *supervisedCmd) Done() <-chan struct{} { return s.done }

func (s *supervisedCmd) Wait() error {
	defer close(s.done)
	kind, value, readErr := s.readStatus()
	_ = s.shim.Wait()
	unregisterReaper(s.shim.Process.Pid)
	s.stopCtx()
	s.Kill()
	_ = s.status.Close()

	var raw int
	if readErr == nil && kind == "exit" {
		raw, readErr = strconv.Atoi(value)
	} else if readErr == nil {
		readErr = fmt.Errorf("unexpected reaper status %q", kind)
	}
	if readErr != nil {
		// The reaper was killed before it could reap the command's
		// descendants, which were reparented to the supervisor. Kill them and
		// exit rather than restart the command or keep its lease.
		killEscapedDescendants()
		log.Fatalf("Reaper of pid %d (%v) exited without reaping its descendants: %v", s.pid, s.path, readErr)
	}
	s.ws = syscall.WaitStatus(raw)
	switch {
	case s.ws.Signaled():
		return fmt.Errorf("signal: %v", s.ws.Signal())
	case s.ws.ExitStatus() != 0:
		return fmt.Errorf("exit status %d", s.ws.ExitStatus())
	}
	return nil
}

func (s *supervisedCmd) Run() error {
	if err := s.Start(); err != nil {
		return err
	}
	return s.Wait()
}

// ExitCode follows os.ProcessState.ExitCode: -1 if the command was killed by
// a signal or has not exited.
func (s *supervisedCmd) ExitCode() int {
	select {
	case <-s.done:
	default:
		return -1
	}
	if s.pid == 0 || !s.ws.Exited() {
		return -1
	}
	return s.ws.ExitStatus()
}

//go:build !linux

package process

import (
	"context"
	"io"
	"os/exec"
	"syscall"
)

// supervisedCmd runs the command directly. Descendants are only contained on
// linux, where a child subreaper is available.
type supervisedCmd struct {
	Env         []string
	Dir         string
	SysProcAttr *syscall.SysProcAttr
	Stdout      io.Writer
	Stderr      io.Writer

	cmd  *exec.Cmd
	done chan struct{}
}

func newSupervisedCmd(ctx context.Context, entrypoint string, args ...string) *supervisedCmd {
	return &supervisedCmd{
		cmd:  exec.CommandContext(ctx, entrypoint, args...), // #nosec G204
		done: make(chan struct{}),
	}
}

func (s *supervisedCmd) StdoutPipe() (io.ReadCloser, error) { return s.cmd.StdoutPipe() }
func (s *supervisedCmd) StderrPipe() (io.ReadCloser, error) { return s.cmd.StderrPipe() }

func (s *supervisedCmd) Start() error {
	s.cmd.Env = s.Env
	s.cmd.Dir = s.Dir
	s.cmd.SysProcAttr = s.SysProcAttr
	if s.Stdout != nil {
		s.cmd.Stdout = s.Stdout
	}
	if s.Stderr != nil {
		s.cmd.Stderr = s.Stderr
	}
	return s.cmd.Start()
}

func (s *supervisedCmd) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func (s *supervisedCmd) Kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (s *supervisedCmd) Done() <-chan struct{} { return s.done }

func (s *supervisedCmd) Wait() error {
	defer close(s.done)
	return s.cmd.Wait()
}

func (s *supervisedCmd) Run() error {
	if err := s.Start(); err != nil {
		return err
	}
	return s.Wait()
}

func (s *supervisedCmd) ExitCode() int { return s.cmd.ProcessState.ExitCode() }

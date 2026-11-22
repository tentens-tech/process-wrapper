package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	ExecProbeTimeout = 30
)

type ExecProbe struct {
	Command Command `yaml:"command"`
	// supervised is the command the probe checks, set by ProbeRunner
	supervised *Command
}

// For returns a copy of the probe bound to the supervised command: the probe
// runs with its credential and working directory.
func (p ExecProbe) For(c *Command) ExecProbe {
	p.supervised = c
	return p
}

func (p ExecProbe) String() string {
	return fmt.Sprintf("Exec %v", p.Command.Entrypoint)
}

func (p ExecProbe) Run(name string) bool {
	logger := log.WithFields(log.Fields{"name": name, "command": p.Command.Entrypoint})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*ExecProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.Command.Entrypoint, p.Command.Args...) // #nosec G204
	// Inherit the wrapper environment (PATH, HOME, ...) and add probe envs on top
	cmd.Env = os.Environ()
	for k, v := range p.Command.Envs {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%v=%v", k, v))
	}
	var supervisedAttr *syscall.SysProcAttr
	if p.supervised != nil {
		cmd.Dir = p.supervised.Dir
		supervisedAttr = p.supervised.SysProcAttr
	}
	if p.Command.Dir != "" {
		cmd.Dir = p.Command.Dir
	}
	// Never run the probe with more privileges than the supervised command,
	// and kill its whole process group on timeout
	cmd.SysProcAttr = childProcAttr(p.Command.SysProcAttr, supervisedAttr)
	cmd.Cancel = func() error { return SignalGroup(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second

	err := cmd.Run()
	// The process exited successfully, but its background jobs kept the
	// output open: not a failure, the jobs are killed below
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if cmd.Process != nil {
		// Don't leave background jobs of the probe running
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		logger.Warnf("Exec probe failure, command execution timeout")
		return false
	}
	if err != nil {
		logger.Warnf("Exec probe failure, %v", err)
		return false
	}
	logger.Debugf("Exec probe success")
	return true
}

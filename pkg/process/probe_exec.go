package process

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"os/exec"
	"time"
)

const (
	ExecProbeTimeout = 30
)

type ExecProbe struct {
	Command Command `yaml:"command"`
	ctx     context.Context
	sandbox sandbox
}

func (p ExecProbe) String() string {
	return fmt.Sprintf("Exec %v", p.Command.String())
}

// bind returns a copy of the probe that runs within the lifetime and the sandbox
// of the supervised command.
func (p ExecProbe) bind(ctx context.Context, sb sandbox) ExecProbe {
	p.ctx = ctx
	p.sandbox = sb
	return p
}

func (p ExecProbe) Run(name string) bool {
	logger := log.WithFields(log.Fields{"name": name, "command": p.Command.String()})
	if p.ctx == nil {
		logger.Errorf("Exec probe failure, probe is not bound to a supervised command")
		return false
	}

	ctx, cancel := context.WithTimeout(p.ctx, time.Second*ExecProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Command.Entrypoint, p.Command.Args...) // #nosec G204
	// A nil Env would make the probe inherit the supervisor environment.
	cmd.Env = make([]string, 0, len(p.Command.Envs))
	for k, v := range p.Command.Envs {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%v=%v", k, v))
	}
	p.sandbox.apply(cmd, p.Command.SysProcAttr)

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Warnf("Exec probe failure, command execution timeout")
			return false
		}
		logger.Warnf("Exec probe failure, %v", err)
		logger.Debugf("Exec probe failure, exit status %v", cmd.ProcessState.ExitCode())
		return false
	}
	logger.Debugf("Exec probe success")
	return true
}

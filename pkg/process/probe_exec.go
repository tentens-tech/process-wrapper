package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	ExecProbeTimeout = 30
)

type ExecProbe struct {
	Command Command `yaml:"command"`
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
	if p.Command.Dir != "" {
		cmd.Dir = p.Command.Dir
	}
	cmd.WaitDelay = time.Second

	err := cmd.Run()
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

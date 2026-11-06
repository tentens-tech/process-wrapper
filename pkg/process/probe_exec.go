package process

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"time"
)

const (
	ExecProbeTimeout = 30
)

type ExecProbe struct {
	Command Command `yaml:"command"`
}

func (p ExecProbe) String() string {
	return fmt.Sprintf("Exec %v", p.Command.String())
}

func (p ExecProbe) Run(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*ExecProbeTimeout)
	defer cancel()
	cmd := newSupervisedCmd(ctx, p.Command.Entrypoint, p.Command.Args...)
	for k, v := range p.Command.Envs {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%v=%v", k, v))
	}
	// Run returns once the probe command and everything it started are gone.
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Warnf("Exec probe failure, command execution timeout")
		return false
	}
	if err != nil {
		log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Warnf("Exec probe failure, %v", err)
	}
	if code := cmd.ExitCode(); code != 0 {
		log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Debugf("Exec probe failure, exit status %v", code)
		return false
	}
	log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Debugf("Exec probe success")
	return true
}

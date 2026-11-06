package process

import (
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	"os/exec"
	"syscall"
	"time"
)

const (
	ExecProbeTimeout = 30
)

type ExecProbe struct {
	Command Command `yaml:"command"`
	// supervised is the SysProcAttr of the command the probe checks.
	supervised *syscall.SysProcAttr
}

func (p ExecProbe) String() string {
	return fmt.Sprintf("Exec %v", p.Command.String())
}

func (p ExecProbe) Run(name string) bool {
	attr, err := childSysProcAttr(p.Command.SysProcAttr, p.supervised)
	if err != nil {
		log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Errorf("Exec probe is not executed: %v", err)
		return false
	}

	exitCode := make(chan int, 1)

	t := time.NewTicker(time.Second * ExecProbeTimeout)

	go func() {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second*ExecProbeTimeout))
		cmd := exec.CommandContext(ctx, p.Command.Entrypoint, p.Command.Args...) // #nosec G204
		cmd.Env = childEnv(p.Command.Envs)
		cmd.SysProcAttr = attr
		err := cmd.Run()
		if err != nil {
			log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Warnf("Exec probe failure, %v", err)
		}
		exitCode <- cmd.ProcessState.ExitCode()
		cancel()
	}()

	for {
		select {
		case <-t.C:
			log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Warnf("Exec probe failure, command execution timeout")
			return false
		case code := <-exitCode:
			if code != 0 {
				log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Debugf("Exec probe failure, exit status %v", code)
				return false
			}
			log.WithFields(log.Fields{"name": name, "command": p.Command.String()}).Debugf("Exec probe success")
			return true
		}
	}
}

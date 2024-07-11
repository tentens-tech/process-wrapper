package process

import (
	"context"
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
}

func (p ExecProbe) String() string {
	return fmt.Sprintf("Exec %v", p.Command.String())
}

func (p ExecProbe) Run(name string) bool {
	exitCode := make(chan int, 1)

	t := time.NewTicker(time.Second * ExecProbeTimeout)

	go func() {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second*ExecProbeTimeout))
		cmd := exec.CommandContext(ctx, p.Command.Entrypoint, p.Command.Args...) // #nosec G204
		for k, v := range p.Command.Envs {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%v=%v", k, v))
		}
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

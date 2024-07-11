package signals

import (
	"context"
	log "github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"process-wrapper/pkg/process"
	"syscall"
	"time"
)

type Notifier struct {
	cancel           context.CancelFunc
	gracefulShutdown time.Duration
	s                *process.State
}

func NewNotifier(cancel context.CancelFunc, gracefulShutdown time.Duration, s *process.State) *Notifier {
	return &Notifier{
		cancel:           cancel,
		gracefulShutdown: gracefulShutdown,
		s:                s,
	}
}

func (n *Notifier) Notify() {
	log.Debugf("Start notify handler")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGUSR2)
	for sig := range sigs {
		switch sig {
		case syscall.SIGTERM, syscall.SIGQUIT:
			n.s.M.PushTerminationEvent(map[string]string{
				"signal": "termination",
			})
			log.Warnf("Received %v, graceful shutdown", sig)
			n.s.CancelRoutines()
			n.s.RemoveReadinessFile()
			n.s.SetShutdown()
			time.Sleep(time.Second)
			log.Warnf("Terminate all commands")
			n.TerminateProcess()
			n.s.PrintState()
			log.Printf("Successfully terminated")
		case syscall.SIGINT:
			n.s.SetShutdown()
			n.s.M.PushTerminationEvent(map[string]string{
				"signal": "interrupt",
			})
			log.Warnf("Received %v, fast shutdown", sig)
			n.s.RemoveReadinessFile()
			n.cancel()
		case syscall.SIGUSR1, syscall.SIGUSR2:
			n.s.M.PushTerminationEvent(map[string]string{
				"signal": "usr",
			})
			log.Warnf("Received %v, print processes state", sig)
			n.s.PrintState()
		}
	}
}

func (n *Notifier) TerminateProcess() {
	var err error
	var process *os.Process
	for _, pid := range n.s.GetPids() {
		process, err = os.FindProcess(pid)
		if err != nil {
			log.Warnf("Cannot find procces with pid: %v, %v", pid, err)
			continue
		}
		log.Warnf("Send SIGTERM to %v", pid)
		err = process.Signal(syscall.SIGTERM)
		if err != nil {
			log.Errorf("Cannot terminate the process %v", pid)
		}
	}
	defer n.ProcessKiller()
}

func (n *Notifier) ProcessKiller() {
	time.Sleep(n.gracefulShutdown)
	n.s.M.PushTerminationEvent(map[string]string{
		"signal": "kill",
	})
	log.Warnf("Force kill all hanged processes")
	n.cancel()
}

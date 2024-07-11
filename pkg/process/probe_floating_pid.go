package process

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"time"
)

const (
	DefaultFloatingPidDurationSeconds = 10
	DefaultFloatingPidThreshold       = 1
)

type FloatingPid struct {
	State     *State
	Period    Duration `yaml:"period"`
	Threshold int      `yaml:"threshold"`
}

func (p FloatingPid) Run(name string) bool {
	if p.Period.Duration.Seconds() == 0 {
		p.Period.Duration = time.Second * DefaultFloatingPidDurationSeconds
	}
	if p.Threshold == 0 {
		p.Threshold = DefaultFloatingPidThreshold
	}
	r := p.State.NumOfRestarts(p.Period.Duration, name)
	log.WithFields(log.Fields{"name": name}).Debugf("Process number of restarts: %v", r)

	return r <= p.Threshold
}

func (p FloatingPid) String() string {
	return fmt.Sprintf("Floating pid, with period: %v, threshold: %v", p.Period, p.Threshold)
}

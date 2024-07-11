package process

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"time"
)

type HeartBeat struct {
	Period Duration `yaml:"period"`
	State  *State   `yaml:"-"`
}

func (h *HeartBeat) String() string {
	return fmt.Sprintf("HeartBeat: %+v", h.Period)
}

func (h *HeartBeat) Run(name string) bool {
	t := h.State.GetHeartBeat(name)
	if time.Since(t) > h.Period.Duration {
		log.WithFields(log.Fields{"name": name}).Errorf("HeartBeat probe failure")
		return false
	}
	log.WithFields(log.Fields{"name": name}).Debugf("HeartBeat probe success")
	return true
}

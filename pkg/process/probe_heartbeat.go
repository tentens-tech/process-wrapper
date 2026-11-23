package process

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultHeartBeatPeriod is used when heartBeat.period isn't set
const DefaultHeartBeatPeriod = 30 * time.Second

type HeartBeat struct {
	Period Duration `yaml:"period"`
	State  *State   `yaml:"-" json:"-"`
}

func (h *HeartBeat) String() string {
	return fmt.Sprintf("HeartBeat: %v", h.period())
}

func (h *HeartBeat) Run(name string) bool {
	t := h.State.GetHeartBeat(name)
	if time.Since(t) > h.period() {
		log.WithFields(log.Fields{"name": name}).Errorf("HeartBeat probe failure")
		return false
	}
	log.WithFields(log.Fields{"name": name}).Debugf("HeartBeat probe success")
	return true
}

func (h *HeartBeat) period() time.Duration {
	if h.Period.Duration <= 0 {
		return DefaultHeartBeatPeriod
	}
	return h.Period.Duration
}

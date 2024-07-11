package process

import (
	"bytes"
	"fmt"
	log "github.com/sirupsen/logrus"
	"net/http"
)

type LockProbe struct {
	Lease       string `yaml:"-"`
	TTLDuration string `yaml:"ttlDuration"`
}

func (p LockProbe) String() string {
	return fmt.Sprintf("Check lease %v", p.Lease)
}

func (p LockProbe) Run(name string) bool {
	url := SharedLockURL + DefaultSharedLockKeepAliveEndpoint
	data := bytes.NewBufferString(p.Lease)
	resp, err := http.Post(url, "text/plain", data)
	if err != nil {
		log.WithFields(log.Fields{"name": name}).Errorf("Failed to keep lease alive: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		log.WithFields(log.Fields{"name": name}).Warnf("Lease is expired")
		return false
	}

	log.WithFields(log.Fields{"name": name}).Debugf("Prolong KeepAlive OK")
	return true
}

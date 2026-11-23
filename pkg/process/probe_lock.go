package process

import (
	"bytes"
	"io"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

const LockProbeTimeout = 5 * time.Second

var lockProbeClient = &http.Client{Timeout: LockProbeTimeout}

type LockProbe struct {
	Lease       string `yaml:"-"`
	TTLDuration string `yaml:"ttlDuration"`
}

// String must not contain the lease ID: it is used as a metric event label
func (p LockProbe) String() string {
	return "Check lease"
}

func (p LockProbe) Run(name string) bool {
	logger := log.WithFields(log.Fields{"name": name, "lease": p.Lease})
	url := SharedLockURL + DefaultSharedLockKeepAliveEndpoint
	resp, err := lockProbeClient.Post(url, "text/plain", bytes.NewBufferString(p.Lease))
	if err != nil {
		logger.Errorf("Failed to keep lease alive: %v", err)
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		logger.Debugf("Prolong KeepAlive OK")
		return true
	case http.StatusNoContent:
		logger.Warnf("Lease is expired")
	default:
		logger.Errorf("Failed to keep lease alive, unexpected status code %v", resp.StatusCode)
	}
	return false
}

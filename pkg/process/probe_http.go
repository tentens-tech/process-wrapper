package process

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
)

type HTTPProbe struct {
	URL string `yaml:"url"`
}

func (p HTTPProbe) String() string {
	return fmt.Sprintf("HTTP Get %v", p.URL)
}

func (p HTTPProbe) Run(name string) bool {
	resp, err := http.Get(p.URL)
	if err != nil {
		log.WithFields(log.Fields{"name": name, "url": p.URL}).Errorf("HTTP probe failure, %v", err)
		return false
	}
	defer func() {
		_, err = io.Copy(io.Discard, resp.Body)
		if err != nil {
			log.WithFields(log.Fields{"name": name, "url": p.URL}).Errorf("Discard response body error, %v", err)
		}
		resp.Body.Close()
	}()
	if resp.StatusCode < 299 {
		log.WithFields(log.Fields{"name": name, "url": p.URL}).Debugf("HTTP probe success")
		return true
	}
	log.WithFields(log.Fields{"name": name, "url": p.URL}).Errorf("HTTP probe failure")
	return false
}

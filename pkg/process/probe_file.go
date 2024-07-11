package process

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"os"
)

type FileProbe struct {
	Path string `yaml:"path"`
}

func (p FileProbe) String() string {
	return fmt.Sprintf("Stat file: %v", p.Path)
}

func (p FileProbe) Run(name string) bool {
	_, err := os.Stat(p.Path)
	if err != nil {
		log.WithFields(log.Fields{"name": name, "path": p.Path}).Errorf("File probe failure")
		return false
	}
	log.WithFields(log.Fields{"name": name, "path": p.Path}).Debugf("File probe success")
	return true
}

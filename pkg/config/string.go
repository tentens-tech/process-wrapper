package config

import (
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	"process-wrapper/pkg/process"
)

type String struct {
	Config         string
	ReloadedConfig []*process.Command
}

func (c String) Load() []process.Command {
	var err error
	var commands []process.Command
	err = yaml.Unmarshal([]byte(c.Config), &commands)
	if err != nil {
		log.Errorf("Unmarshal string config error, %v", err)
	}
	log.Printf("Loaded %v commands from string config: \n\n %+v \n\n", len(commands), commands)
	return commands
}

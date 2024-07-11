package config

import (
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"os"
	"process-wrapper/pkg/process"
)

const (
	ConfigEnv = "CONFIG"
)

type JSONEnv struct {
	Env string
}

func (c JSONEnv) Load() []process.Command {
	var err error
	var commands []process.Command
	configJSONString := os.Getenv(ConfigEnv)
	if configJSONString == "" {
		log.Errorf("Cannot load json config from %v env", ConfigEnv)
	}
	log.Printf("Loaded commands form json env: %v", configJSONString)
	err = json.Unmarshal([]byte(configJSONString), &commands)
	if err != nil {
		log.Errorf("Unmarshal json env config error, %v", err)
	}
	log.Debugf("Loaded %v commands from json env config: \n\n %+v \n\n", len(commands), commands)
	return commands
}

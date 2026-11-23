package config

import (
	"encoding/json"
	"os"
	"process-wrapper/pkg/process"

	log "github.com/sirupsen/logrus"
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
	err = json.Unmarshal([]byte(configJSONString), &commands)
	if err != nil {
		log.Errorf("Unmarshal json env config error, %v", err)
	}
	// Don't log the config itself: it may contain secrets in envs
	log.Printf("Loaded %v commands from %v env", len(commands), ConfigEnv)
	return commands
}

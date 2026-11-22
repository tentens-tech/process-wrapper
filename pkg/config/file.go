package config

import (
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	"os"
	"process-wrapper/pkg/process"
	"strings"
)

const (
	EnvPrefix = "SIDECAR"
)

type File struct {
	Path string
	Env  map[string]string
}

func (cf File) Load() []process.Command {
	cf.Env = make(map[string]string)
	for _, envs := range os.Environ() {
		k, v, ok := strings.Cut(envs, "=")
		if ok && strings.Contains(k, EnvPrefix) {
			cf.Env[k] = v
		}
	}
	// Log only variable names: values may contain secrets
	for k := range cf.Env {
		log.Printf("Found env %v", k)
	}

	log.Printf("Load config %v", cf.Path)
	var (
		err    error
		config []byte
	)
	config, err = os.ReadFile(cf.Path) // #nosec G304 -- config path is provided by the operator
	if err != nil {
		log.Errorf("Cannot read config file %v, %v", cf.Path, err)
	}

	var commands []process.Command
	err = yaml.Unmarshal(config, &commands)
	if err != nil {
		log.Fatalf("Unmarshal yaml config %v error, %v", cf.Path, err)
	}
	log.Debugf("Load comands: %+v", commands)
	return commands
}

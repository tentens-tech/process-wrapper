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
		kv := strings.Split(envs, "=")
		if len(kv) == 2 {
			if strings.Contains(kv[0], EnvPrefix) {
				cf.Env[kv[0]] = kv[1]
			}
		}
	}
	for k, v := range cf.Env {
		log.Printf("%v: %v", k, v)
	}

	log.Printf("Load config %v", cf.Path)
	var (
		err    error
		config []byte
	)
	config, err = os.ReadFile(cf.Path)
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

package config

import (
	"encoding/json"
	"process-wrapper/pkg/process"
	"process-wrapper/pkg/prommerge"
)

type Config interface {
	Load() []process.Command
}

func ToJSON(commands []process.Command) ([]byte, error) {
	data, err := json.Marshal(commands)
	if err != nil {
		return nil, err
	}
	return data, err
}

func PromTargets(commands []process.Command) []prommerge.Target {
	var targets []prommerge.Target
	for i := range commands {
		if commands[i].PromTarget != "" {
			targets = append(targets, prommerge.Target{
				Name:        commands[i].Name,
				URL:         commands[i].PromTarget,
				ExtraLabels: map[string]string{"process_name": commands[i].Name},
			})
		}
	}
	return targets
}

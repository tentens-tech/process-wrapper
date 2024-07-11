package config

import (
	"encoding/json"
	"fmt"
	"github.com/username1366/prommerge"
	"process-wrapper/pkg/process"
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

func PromTargets(commands []process.Command) []prommerge.PromTarget {
	var targets []prommerge.PromTarget
	for i, _ := range commands {
		if commands[i].PromTarget != "" {
			targets = append(targets, prommerge.PromTarget{
				Name: commands[i].Name,
				Url:  commands[i].PromTarget,
				ExtraLabels: []string{
					fmt.Sprintf("process_name=%v", commands[i].Name),
				},
			})
		}
	}
	return targets
}

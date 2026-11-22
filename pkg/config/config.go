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
			target := prommerge.Target{
				Name:        commands[i].Name,
				URL:         commands[i].PromTarget,
				ExtraLabels: map[string]string{"process_name": commands[i].Name},
			}
			// Replicas share the target, it is scraped while any of them is ready
			for r := 0; r < commands[i].Replicas; r++ {
				target.Processes = append(target.Processes, process.ReplicaName(commands[i].Name, r))
			}
			targets = append(targets, target)
		}
	}
	return targets
}

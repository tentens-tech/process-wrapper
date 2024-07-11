package process

import (
	"fmt"
	"os"
)

type EnvProbe struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

func (p EnvProbe) String() string {
	return fmt.Sprintf("Check env %v:%v", p.Name, p.Value)
}

func (p EnvProbe) Run(_ string) bool {
	return os.Getenv(p.Name) == p.Value
}

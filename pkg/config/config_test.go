package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"process-wrapper/pkg/process"
)

const envsTestConfig = `
- name: "envs"
  entrypoint: "/bin/sleep"
  args: ["1"]
  envs:
    SERVER: "1"
  envsFile: "{{ENVS_FILE}}"
`

func TestMergeEnvsFromFile(t *testing.T) {
	envsFile := filepath.Join(t.TempDir(), "envs")
	if err := os.WriteFile(envsFile, []byte("FOO=bar\nQUOTED=\"baz\"\n\nWITH_EQ=a=b\n"), 0o600); err != nil {
		t.Fatalf("Failed to create envs file, %v", err)
	}

	var commands []process.Command
	cfg := strings.ReplaceAll(envsTestConfig, "{{ENVS_FILE}}", envsFile)
	if err := yaml.Unmarshal([]byte(cfg), &commands); err != nil {
		t.Fatalf("Failed to parse config, %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	envs, err := commands[0].MergeEnvs()
	if err != nil {
		t.Fatalf("MergeEnvs: %v", err)
	}
	want := map[string]string{
		"SERVER":  "1",
		"FOO":     "bar",
		"QUOTED":  "baz",
		"WITH_EQ": "a=b",
	}
	for k, v := range want {
		if envs[k] != v {
			t.Errorf("env %s: expected %q, got %q", k, v, envs[k])
		}
	}
}

func TestToJSON(t *testing.T) {
	var commands []process.Command
	cfg := `
- name: "floating"
  entrypoint: "/bin/true"
  readiness:
    floatingPid: {}
`
	if err := yaml.Unmarshal([]byte(cfg), &commands); err != nil {
		t.Fatalf("Failed to parse config, %v", err)
	}
	// Runtime state holds a context.CancelFunc, which encoding/json cannot marshal
	commands[0].Readiness.FloatingPid.State = &process.State{PreemptedCancel: func() {}}

	if _, err := ToJSON(commands); err != nil {
		t.Fatalf("ToJSON failed, %v", err)
	}
}

func TestPromTargets(t *testing.T) {
	commands := []process.Command{
		{Name: "a", PromTarget: "http://localhost:9090/metrics"},
		{Name: "b"},
	}
	targets := PromTargets(commands)
	if len(targets) != 1 || targets[0].Name != "a" {
		t.Fatalf("unexpected targets %+v", targets)
	}
	targets = PromTargets([]process.Command{{Name: "r", PromTarget: "http://t", Replicas: 2}})
	if len(targets) != 1 || !reflect.DeepEqual(targets[0].Processes, []string{"r_0", "r_1"}) {
		t.Fatalf("replicas must gate their shared target, got %+v", targets)
	}
}

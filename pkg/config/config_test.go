package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// TestCommandLogsField verifies that the per-command `logs` flag is parsed
// from YAML and that LogsEnabled() returns the expected default and overrides.
func TestCommandLogsField(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSet bool // whether Logs pointer must be non-nil after unmarshal
		wantVal bool // dereferenced value when wantSet is true
		wantOn  bool // expected LogsEnabled() result
	}{
		{
			name: "Unset defaults to enabled",
			yaml: `
- name: "quiet-sidecar"
  entrypoint: "/usr/bin/some-tool"
  args: ["--verbose"]
`,
			wantSet: false,
			wantOn:  true,
		},
		{
			name: "Explicit false disables logs",
			yaml: `
- name: "noisy-sidecar"
  entrypoint: "/usr/bin/some-tool"
  logs: false
  args: ["--verbose"]
`,
			wantSet: true,
			wantVal: false,
			wantOn:  false,
		},
		{
			name: "Explicit true keeps logs enabled",
			yaml: `
- name: "verbose-sidecar"
  entrypoint: "/usr/bin/some-tool"
  logs: true
`,
			wantSet: true,
			wantVal: true,
			wantOn:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var commands []process.Command
			if err := yaml.Unmarshal([]byte(tc.yaml), &commands); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if len(commands) != 1 {
				t.Fatalf("expected 1 command, got %d", len(commands))
			}
			cmd := commands[0]

			if tc.wantSet {
				if cmd.Logs == nil {
					t.Fatalf("expected Logs to be set, got nil")
				}
				if *cmd.Logs != tc.wantVal {
					t.Errorf("expected *Logs=%v, got %v", tc.wantVal, *cmd.Logs)
				}
			} else if cmd.Logs != nil {
				t.Errorf("expected Logs to be nil, got %v", *cmd.Logs)
			}

			if got := cmd.LogsEnabled(); got != tc.wantOn {
				t.Errorf("LogsEnabled() = %v, want %v", got, tc.wantOn)
			}
		})
	}
}

// TestCommandLogLevelsYAML verifies that the per-command `logLevels` list
// is parsed from YAML. Empty/missing fields fall back to the env filter at
// runtime, which is covered in the process package tests.
func TestCommandLogLevelsYAML(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "Unset",
			yaml: `
- name: "no-filter"
  entrypoint: "/usr/bin/some-tool"
`,
			want: nil,
		},
		{
			name: "Explicit warning+error",
			yaml: `
- name: "noisy-sidecar"
  entrypoint: "/usr/bin/some-tool"
  logLevels:
    - warning
    - error
`,
			want: []string{"warning", "error"},
		},
		{
			name: "Flow style",
			yaml: `
- name: "noisy-sidecar"
  entrypoint: "/usr/bin/some-tool"
  logLevels: ["warning", "error"]
`,
			want: []string{"warning", "error"},
		},
		{
			name: "Empty list",
			yaml: `
- name: "noisy-sidecar"
  entrypoint: "/usr/bin/some-tool"
  logLevels: []
`,
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var commands []process.Command
			if err := yaml.Unmarshal([]byte(tc.yaml), &commands); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if len(commands) != 1 {
				t.Fatalf("expected 1 command, got %d", len(commands))
			}
			got := commands[0].LogLevels

			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil LogLevels, got %v", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("LogLevels size mismatch: got %v want %v", got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Errorf("LogLevels[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestCommandRestartBackoffYAML covers the round-trip parsing of the
// per-command `restartBackoff` block including partial overrides and an
// empty/missing block (must yield a nil pointer so defaults can apply at
// runtime).
func TestCommandRestartBackoffYAML(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantNil  bool
		wantInit time.Duration
		wantMax  time.Duration
		wantMul  float64
		wantSucc time.Duration
		wantJit  float64
	}{
		{
			name: "Unset",
			yaml: `
- name: "no-backoff"
  entrypoint: "/usr/bin/some-tool"
`,
			wantNil: true,
		},
		{
			name: "Full override",
			yaml: `
- name: "flaky-worker"
  entrypoint: "/usr/bin/worker"
  restartBackoff:
    initialDelay: 500ms
    maxDelay: 30s
    multiplier: 3
    successThreshold: 5s
    jitter: 0.1
`,
			wantInit: 500 * time.Millisecond,
			wantMax:  30 * time.Second,
			wantMul:  3,
			wantSucc: 5 * time.Second,
			wantJit:  0.1,
		},
		{
			name: "Partial override (defaults applied via WithDefaults)",
			yaml: `
- name: "flaky-worker"
  entrypoint: "/usr/bin/worker"
  restartBackoff:
    initialDelay: 2s
`,
			wantInit: 2 * time.Second,
			// Other fields remain zero in the YAML — WithDefaults fills them.
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var commands []process.Command
			if err := yaml.Unmarshal([]byte(tc.yaml), &commands); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if len(commands) != 1 {
				t.Fatalf("expected 1 command, got %d", len(commands))
			}
			got := commands[0].RestartBackoff
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected RestartBackoff nil, got %+v", got)
				}
				// And ResolveBackoff() must still produce defaults so the
				// runtime never sees zeros.
				resolved := commands[0].ResolveBackoff()
				if resolved.InitialDelay.Duration <= 0 || resolved.MaxDelay.Duration <= 0 {
					t.Errorf("resolved defaults missing: %+v", resolved)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected RestartBackoff non-nil")
			}
			if tc.wantInit != 0 && got.InitialDelay.Duration != tc.wantInit {
				t.Errorf("InitialDelay = %v, want %v", got.InitialDelay.Duration, tc.wantInit)
			}
			if tc.wantMax != 0 && got.MaxDelay.Duration != tc.wantMax {
				t.Errorf("MaxDelay = %v, want %v", got.MaxDelay.Duration, tc.wantMax)
			}
			if tc.wantMul != 0 && got.Multiplier != tc.wantMul {
				t.Errorf("Multiplier = %v, want %v", got.Multiplier, tc.wantMul)
			}
			if tc.wantSucc != 0 && got.SuccessThreshold.Duration != tc.wantSucc {
				t.Errorf("SuccessThreshold = %v, want %v", got.SuccessThreshold.Duration, tc.wantSucc)
			}
			if tc.wantJit != 0 && got.Jitter != tc.wantJit {
				t.Errorf("Jitter = %v, want %v", got.Jitter, tc.wantJit)
			}
		})
	}
}

// TestCommandRestartPolicyYAML verifies that `restartPolicy` is parsed
// as-is from YAML (case preserved) and that the runtime normalisation
// applied inside Run() accepts case-insensitive values plus the
// historical alias.
func TestCommandRestartPolicyYAML(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "Unset",
			yaml: `
- name: "no-policy"
  entrypoint: "/usr/bin/some-tool"
`,
			want: "",
		},
		{
			name: "Always",
			yaml: `
- name: "always-policy"
  entrypoint: "/usr/bin/some-tool"
  restartPolicy: always
`,
			want: "always",
		},
		{
			name: "Never lowercase",
			yaml: `
- name: "never-policy"
  entrypoint: "/usr/bin/some-tool"
  restartPolicy: never
`,
			want: "never",
		},
		{
			name: "NEVER mixed case (preserved verbatim)",
			yaml: `
- name: "never-policy"
  entrypoint: "/usr/bin/some-tool"
  restartPolicy: NEVER
`,
			want: "NEVER",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var commands []process.Command
			if err := yaml.Unmarshal([]byte(tc.yaml), &commands); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if len(commands) != 1 {
				t.Fatalf("expected 1 command, got %d", len(commands))
			}
			if got := commands[0].RestartPolicy; got != tc.want {
				t.Errorf("RestartPolicy = %q, want %q", got, tc.want)
			}
		})
	}
}

// An omitted jitter gets the default, an explicit 0 disables it
func TestRestartBackoffJitterYAML(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want float64
	}{
		{"restartBackoff:\n  initialDelay: 2s\n", process.DefaultRestartBackoffJitter},
		{"restartBackoff:\n  jitter: 0\n", 0},
		{"restartBackoff:\n  jitter: 0.5\n", 0.5},
		{"entrypoint: x\n", process.DefaultRestartBackoffJitter},
	} {
		var c process.Command
		if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
			t.Fatalf("yaml.Unmarshal: %v", err)
		}
		if got := c.ResolveBackoff().Jitter; got != tc.want {
			t.Errorf("%q: jitter %v, want %v", tc.yaml, got, tc.want)
		}
	}
}

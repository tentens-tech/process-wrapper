package config

import (
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"process-wrapper/pkg/process"
)

// checkAllFields asserts every field of testdata/all-fields.yaml, so a renamed
// yaml/json key or a broken Duration parser is caught
func checkAllFields(t *testing.T, commands []process.Command) {
	t.Helper()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %v", len(commands))
	}
	c := commands[0]
	eq := func(field string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%v: got %#v, want %#v", field, got, want)
		}
	}
	eq("name", c.Name, "app")
	eq("entrypoint", c.Entrypoint, "sh")
	eq("args", c.Args, []string{"-c", "exec sleep 3600"})
	eq("dir", c.Dir, "/srv/app")
	eq("envs", c.Envs, map[string]string{"PORT": "8080"})
	eq("envsFile", c.EnvsFile, "/etc/app/envs")
	eq("restartPolicy", c.RestartPolicy, "Always")
	eq("logFormat", c.LogFormat, "auto")
	eq("replicas", c.Replicas, 2)
	eq("memoryLimitMB", c.MemoryLimitMB, 512.5)
	eq("terminationGracePeriod", c.TerminationGracePeriod.Duration, 15*time.Second)
	eq("promTarget", c.PromTarget, "http://localhost:8080/metrics")
	if a := c.SysProcAttr; a == nil || !a.Setpgid || a.Credential == nil {
		t.Fatalf("sysProcAttr: got %+v", a)
	}
	eq("sysProcAttr.credential", *c.SysProcAttr.Credential, syscall.Credential{Uid: 1000, Gid: 1001, Groups: []uint32{1002}})

	r := c.Readiness
	if r == nil {
		t.Fatal("readiness is not parsed")
	}
	eq("readiness.delay", r.Delay.Duration, time.Second)
	eq("readiness.period", r.Period.Duration, 2*time.Second)
	eq("readiness.startupDeadline", r.StartupDeadline.Duration, 3*time.Minute)
	eq("readiness.failureThreshold", r.FailureThreshold, 4)
	if r.HTTP == nil || r.TCP == nil || r.File == nil || r.Env == nil || r.Exec == nil || r.SQL == nil ||
		r.AMQP == nil || r.FloatingPid == nil || r.HeartBeat == nil || r.KeepSharedLock == nil {
		t.Fatalf("not every probe is parsed: %+v", r)
	}
	eq("http.url", r.HTTP.URL, "http://localhost:8080/health")
	eq("tcp.socket", r.TCP.Socket, "localhost:8080")
	eq("file.path", r.File.Path, "/tmp/app.ready")
	eq("env", *r.Env, process.EnvProbe{Name: "PROBE_ENV", Value: "enabled"})
	eq("exec.command.entrypoint", r.Exec.Command.Entrypoint, "test")
	eq("exec.command.args", r.Exec.Command.Args, []string{"-f", "/tmp/app.ready"})
	eq("exec.command.dir", r.Exec.Command.Dir, "/tmp")
	eq("exec.command.envs", r.Exec.Command.Envs, map[string]string{"EXPECTED": "ok"})
	eq("sql.queries", r.SQL.Queries, []string{"SELECT 1"})
	eq("sql.contains", r.SQL.Contains, []string{"1"})
	eq("amqp.reuseConn", r.AMQP.ReuseConn, true)
	eq("floatingPid.period", r.FloatingPid.Period.Duration, 20*time.Second)
	eq("floatingPid.threshold", r.FloatingPid.Threshold, 2)
	eq("heartBeat.period", r.HeartBeat.Period.Duration, 45*time.Second)
	eq("lock.ttlDuration", r.KeepSharedLock.TTLDuration, "5s")

	h := c.PostStart
	if h == nil || h.SQL == nil || h.Command == nil {
		t.Fatalf("postStart is not parsed: %+v", h)
	}
	eq("postStart.deadline", h.Deadline.Duration, 10*time.Second)
	eq("postStart.envs", h.Envs, map[string]string{"HOOK_ENV": "hook"})
	eq("postStart.sql.queries", h.SQL.Queries, []string{"SET GLOBAL read_only = 0"})
	eq("postStart.command.entrypoint", h.Command.Entrypoint, "sh")
	eq("postStart.command.args", h.Command.Args, []string{"-c", "echo started"})
	eq("postStart.command.envs", h.Command.Envs, map[string]string{"HOOK_CMD_ENV": "cmd"})
}

func TestFileLoadAllFields(t *testing.T) {
	checkAllFields(t, File{Path: "testdata/all-fields.yaml"}.Load())
}

// The CONFIG env holds the same structure in JSON
func TestJSONEnvLoadAllFields(t *testing.T) {
	commands := File{Path: "testdata/all-fields.yaml"}.Load()
	data, err := ToJSON(commands)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigEnv, string(data))
	checkAllFields(t, JSONEnv{Env: ConfigEnv}.Load())
}

// JSON keys are written in the yaml (camelCase) style in docs and examples
func TestJSONEnvCamelCaseKeys(t *testing.T) {
	t.Setenv(ConfigEnv, `[{"name":"j","entrypoint":"sleep","args":["1"],"envsFile":"/e","terminationGracePeriod":"3s",
		"memoryLimitMB":10,"promTarget":"http://t","readiness":{"delay":"1s","period":"2s","failureThreshold":5,
		"heartBeat":{"period":"7s"},"floatingPid":{"period":"9s","threshold":3},"lock":{"ttlDuration":"4s"}},
		"postStart":{"deadline":"6s","command":{"entrypoint":"true"}}}]`)
	commands := JSONEnv{Env: ConfigEnv}.Load()
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %v", len(commands))
	}
	c := commands[0]
	r := c.Readiness
	if c.EnvsFile != "/e" || c.TerminationGracePeriod.Duration != 3*time.Second || c.MemoryLimitMB != 10 ||
		c.PromTarget != "http://t" || r.Delay.Duration != time.Second || r.Period.Duration != 2*time.Second ||
		r.FailureThreshold != 5 || r.HeartBeat.Period.Duration != 7*time.Second ||
		r.FloatingPid.Period.Duration != 9*time.Second || r.FloatingPid.Threshold != 3 ||
		r.KeepSharedLock.TTLDuration != "4s" || c.PostStart.Deadline.Duration != 6*time.Second ||
		c.PostStart.Command.Entrypoint != "true" {
		t.Errorf("unexpected command %+v, readiness %+v", c, r)
	}
}

func TestJSONEnvInvalid(t *testing.T) {
	t.Setenv(ConfigEnv, `{not json`)
	if commands := (JSONEnv{Env: ConfigEnv}).Load(); len(commands) != 0 {
		t.Errorf("invalid json must not produce commands, got %+v", commands)
	}
}

func TestFileLoadMissingFile(t *testing.T) {
	if commands := (File{Path: filepath.Join(t.TempDir(), "nope.yaml")}).Load(); len(commands) != 0 {
		t.Errorf("missing file must produce no commands, got %+v", commands)
	}
}

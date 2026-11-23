package process

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// stubJobFailed replaces os.Exit for a failed job and records the exit code
func stubJobFailed(t *testing.T) *int {
	t.Helper()
	code := -1
	old := jobFailed
	jobFailed = func(c int) { code = c }
	t.Cleanup(func() { jobFailed = old })
	return &code
}

func TestJobCompletedIsNotRestarted(t *testing.T) {
	code := stubJobFailed(t)
	c := &Command{Name: "job", Entrypoint: "true", IsJob: true, s: newTestState()}
	if c.runOnce(context.Background()) {
		t.Fatal("a completed job must not be restarted")
	}
	if *code != -1 {
		t.Fatalf("a completed job must not stop the wrapper, exit code %v", *code)
	}
}

func TestJobFailureStopsWrapper(t *testing.T) {
	code := stubJobFailed(t)
	c := &Command{Name: "job", Entrypoint: "false", IsJob: true, s: newTestState()}
	if c.runOnce(context.Background()) {
		t.Fatal("a failed job must not be restarted")
	}
	if *code != 1 {
		t.Fatalf("a failed job must stop the wrapper with exit code 1, got %v", *code)
	}
}

// A job killed by the wrapper (failed probe, memory limit) is restarted
func TestKilledJobIsRestarted(t *testing.T) {
	code := stubJobFailed(t)
	s := newTestState()
	c := &Command{
		Name: "job", Entrypoint: "sleep", Args: []string{"300"}, IsJob: true,
		RestartBackoff: &RestartBackoff{InitialDelay: Duration{time.Millisecond}},
		s:              s,
	}
	done := make(chan bool)
	go func() { done <- c.runOnce(context.Background()) }()
	waitFor(t, "job started", func() bool { return s.GetPid("job") != 0 })
	s.SetKillState("job", true)
	c.terminateProcess(log.WithField("test", t.Name()), s.GetPid("job"))
	if !<-done {
		t.Fatal("a job killed by the wrapper must be restarted")
	}
	if *code != -1 {
		t.Fatalf("a killed job must not stop the wrapper, exit code %v", *code)
	}
}

func TestRestartPolicyNever(t *testing.T) {
	for _, policy := range []string{"never", "Never", "newer"} {
		c := &Command{Name: "once", Entrypoint: "false", RestartPolicy: policy, s: newTestState()}
		if c.runOnce(context.Background()) {
			t.Errorf("restartPolicy %q: the command must not be restarted", policy)
		}
	}
	c := &Command{
		Name: "always", Entrypoint: "false",
		RestartBackoff: &RestartBackoff{InitialDelay: Duration{time.Millisecond}},
		s:              newTestState(),
	}
	if !c.runOnce(context.Background()) {
		t.Error("default restart policy: the command must be restarted")
	}
}

// SIGTERM during a long back-off aborts the restart at once
func TestRestartBackoffInterruptedByShutdown(t *testing.T) {
	c := &Command{
		Name: "app", Entrypoint: "false",
		RestartBackoff: &RestartBackoff{InitialDelay: Duration{time.Hour}, MaxDelay: Duration{time.Hour}},
		s:              newTestState(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- c.runOnce(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case restart := <-done:
		if restart {
			t.Fatal("cancelled back-off must not restart the command")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("back-off is not interrupted by the context")
	}
}

func TestLogsDisabled(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	s := newTestState()
	s.SetShutdown() // don't restart
	off := false
	c := &Command{Name: "quiet", Entrypoint: "sh", Args: []string{"-c", "echo forwarded-line"}, Logs: &off, s: s}
	c.runOnce(context.Background())
	on := true
	c2 := &Command{Name: "loud", Entrypoint: "sh", Args: []string{"-c", "echo forwarded-line"}, Logs: &on, s: s}
	c2.runOnce(context.Background())
	var quiet, loud int
	for _, e := range hook.AllEntries() {
		if !strings.Contains(e.Message, "forwarded-line") {
			continue
		}
		switch e.Data["name"] {
		case "quiet":
			quiet++
		case "loud":
			loud++
		}
	}
	if quiet != 0 || loud != 1 {
		t.Fatalf("logs: false must drop the output, logs: true keep it (quiet %v, loud %v)", quiet, loud)
	}
}

func TestLogLevelsFilter(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	c := &Command{Name: "app", LogLevels: []string{"warning", "error"}}
	for _, line := range []string{
		`level=info msg="info line"`,
		`level=warning msg="warning line"`,
		`level=error msg="error line"`,
	} {
		c.logOutputLine(line, "", 1)
	}
	c.LogFormat = "auto"
	c.logOutputLine("plain INFO line", "auto", 1)
	c.logOutputLine("ERROR auto line", "auto", 1)
	c.logOutputLine("raw free line", "free", 1)

	var got []string
	for _, e := range hook.AllEntries() {
		got = append(got, e.Message)
	}
	want := []string{"warning line", "error line", "ERROR auto line"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("filtered lines %q, want %q", got, want)
	}
}

func TestDNSProbe(t *testing.T) {
	p := &DNSProbe{URL: "localhost"}
	if !p.Run("app") {
		t.Fatal("first run records the address list")
	}
	if !p.Run("app") {
		t.Fatal("unchanged address list must pass")
	}
	p.addressList = []net.IP{net.ParseIP("192.0.2.1")}
	if p.Run("app") {
		t.Fatal("changed address list must fail")
	}
	if p.Run("app") {
		t.Fatal("the probe keeps failing until the process is restarted")
	}
	p.reset()
	if !p.Run("app") {
		t.Fatal("after a restart the new address list is recorded")
	}
	if (&DNSProbe{URL: "nonexistent.invalid"}).Run("app") {
		t.Fatal("unresolvable name must fail")
	}
}

// The DNS probe is re-armed on every process start, so a changed address
// list restarts the process once instead of in a loop
func TestDNSProbeResetOnStart(t *testing.T) {
	s := newTestState()
	c := &Command{
		Name: "app",
		s:    s,
		Readiness: &Probe{
			Delay:            Duration{time.Millisecond},
			Period:           Duration{10 * time.Millisecond},
			FailureThreshold: 1000,
			DNS:              &DNSProbe{URL: "localhost", addressList: []net.IP{net.ParseIP("192.0.2.1")}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.HealthChecker(ctx, cancel, 0, nil)
	waitFor(t, "ready", func() bool { return s.ProcessState()["app"] })
}

func TestProbeCloneCopiesDNS(t *testing.T) {
	orig := &Probe{DNS: &DNSProbe{URL: "localhost"}}
	cp := orig.clone()
	if cp.DNS == orig.DNS || cp.DNS.URL != "localhost" {
		t.Error("DNS probe must be copied")
	}
}

func TestReplicaCopiesNewFields(t *testing.T) {
	off := false
	c := &Command{
		Name: "app", Replicas: 2, IsJob: true, Logs: &off, LogLevels: []string{"error"},
		RestartPolicy: "never", RestartBackoff: &RestartBackoff{InitialDelay: Duration{time.Second}},
	}
	r := c.replica(1, newTestState())
	if !r.IsJob || r.Logs == nil || *r.Logs || len(r.LogLevels) != 1 || r.RestartPolicy != "never" ||
		r.RestartBackoff == nil || r.RestartBackoff.InitialDelay.Duration != time.Second {
		t.Fatalf("replica lost settings: %+v", r)
	}
}

func TestLogsEnabledDefault(t *testing.T) {
	if !(&Command{}).LogsEnabled() {
		t.Error("logs are enabled by default")
	}
}

// A job stopped by a graceful shutdown doesn't stop the wrapper with exit code 1
func TestJobStoppedOnShutdown(t *testing.T) {
	code := stubJobFailed(t)
	s := newTestState()
	c := &Command{Name: "job", Entrypoint: "sleep", Args: []string{"300"}, IsJob: true, s: s}
	done := make(chan bool)
	go func() { done <- c.runOnce(context.Background()) }()
	waitFor(t, "job started", func() bool { return s.GetPid("job") != 0 })
	s.SetShutdown()
	c.terminateProcess(log.WithField("test", t.Name()), s.GetPid("job"))
	if <-done {
		t.Fatal("a job stopped on shutdown must not be restarted")
	}
	if *code != -1 {
		t.Fatalf("a job stopped on shutdown must not stop the wrapper, exit code %v", *code)
	}
}

// A ready process resets the restart back-off to initialDelay
func TestReadinessResetsRestartBackoff(t *testing.T) {
	s := newTestState()
	c := &Command{
		Name: "app",
		s:    s,
		RestartBackoff: &RestartBackoff{
			InitialDelay: Duration{time.Second}, MaxDelay: Duration{time.Minute},
			SuccessThreshold: Duration{time.Hour},
		},
		Readiness: &Probe{
			Delay:            Duration{time.Millisecond},
			Period:           Duration{10 * time.Millisecond},
			FailureThreshold: 3,
			DNS:              &DNSProbe{URL: "localhost"},
		},
	}
	c.NextRestartDelay()
	c.NextRestartDelay()
	if n := c.FailureCount(); n != 2 {
		t.Fatalf("failure count %v, want 2", n)
	}
	c.ArmRestartBackoffReset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.HealthChecker(ctx, cancel, 0, nil)
	waitFor(t, "back-off reset", func() bool { return c.FailureCount() == 0 })
	if d := c.NextRestartDelay(); d != time.Second {
		t.Fatalf("delay after reset %v, want initialDelay 1s", d)
	}
}

func TestDNSProbeTimeout(t *testing.T) {
	if (&DNSProbe{}).timeout() != DefaultDNSProbeTimeout {
		t.Error("default DNS lookup timeout")
	}
	if (&DNSProbe{Timeout: Duration{time.Second}}).timeout() != time.Second {
		t.Error("configured DNS lookup timeout")
	}
}

// A broker that accepts TCP but never answers the AMQP handshake fails the
// probe after operationsTimeout instead of hanging the health checker
func TestAMQPProbeHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	p := &AMQPProbe{
		OperationsTimeout: Duration{300 * time.Millisecond},
		Envs: map[string]string{
			AMQPProbeDsnTemplateEnvName:  "amqp://%v:%v@" + ln.Addr().String(),
			AMQPProbeRabbitMqUserEnvName: "u",
			AMQPProbeRabbitMqPassEnvName: "p",
		},
	}
	done := make(chan bool)
	go func() { done <- p.Run("app") }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a silent broker must fail the probe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the AMQP handshake is not bounded by operationsTimeout")
	}
}

package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"process-wrapper/pkg/metrics"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logfmt/logfmt"
	log "github.com/sirupsen/logrus"
)

const (
	ProbePeriodSeconds            = 5
	ProbeDelaySeconds             = 5
	ProbeFailureThreshold         = 3
	DefaultTerminationGracePeriod = 10 * time.Second
	// MaxOutputLineBytes bounds how much of one output line of a command is
	// held in memory, longer lines are logged in parts
	MaxOutputLineBytes = 64 * 1024
	// StartRetryMinDelay and StartRetryMaxDelay bound the backoff between
	// attempts to start a command that fails to start
	StartRetryMinDelay                 = time.Second
	StartRetryMaxDelay                 = 30 * time.Second
	terminationPollInterval            = 100 * time.Millisecond
	DefaultSharedLockLeaseEndpoint     = "/lease"
	DefaultSharedLockKeepAliveEndpoint = "/keepalive"
	RestartPolicyAlways                = "always"
	RestartPolicyNewer                 = "never"
)

var (
	SharedLockURL string
)

// Command represents process entity
type Command struct {
	Name       string            `yaml:"name"`
	Entrypoint string            `yaml:"entrypoint"`
	Args       []string          `yaml:"args"`
	Envs       map[string]string `yaml:"envs"`
	EnvsFile   string            `yaml:"envsFile"`
	// RestartPolicy controls whether a terminated command is restarted.
	// Accepts "always" (default) and "never" — case-insensitive, with the
	// historical typo "newer" honoured as alias. Empty means "always".
	RestartPolicy string `yaml:"restartPolicy"`
	// RestartBackoff configures the exponential back-off delay between
	// automatic restarts. Optional; missing or zero fields fall back to
	// package defaults (1s → 2x → 1m, ±20% jitter, 10s success threshold).
	RestartBackoff *RestartBackoff      `yaml:"restartBackoff"`
	Readiness      *Probe               `yaml:"readiness"`
	LogFormat      string               `yaml:"logFormat"`
	Dir            string               `yaml:"dir"`
	PromTarget     string               `yaml:"promTarget"`
	SysProcAttr    *syscall.SysProcAttr `yaml:"sysProcAttr"`
	Replicas       int                  `yaml:"replicas"`
	PostStart      *Hook                `yaml:"postStart"`
	MemoryLimitMB  float64              `yaml:"memoryLimitMB"`
	// IsJob marks a command that runs to completion: it isn't restarted after
	// exit code 0, a non-zero exit code stops process-wrapper with exit code 1
	IsJob bool `yaml:"isJob"`
	// Logs: false disables forwarding of stdout/stderr of the command
	Logs *bool `yaml:"logs"`
	// LogLevels is a per-command allow-list of log levels forwarded from the
	// child process stdout/stderr. Supported values: "debug", "info",
	// "warning" (alias "warn"), "error". Empty/nil falls back to the global
	// LOG_LEVELS env; if both are empty the filter is disabled. The filter
	// applies only to logfmt-parsed levels and to LOG_FORMAT=auto severity
	// detection; LOG_FORMAT=free/plain is not affected.
	LogLevels []string `yaml:"logLevels"`
	// TerminationGracePeriod is the time between SIGTERM and SIGKILL when the
	// process is restarted by a failed probe or postStart hook (default 10s)
	TerminationGracePeriod Duration `yaml:"terminationGracePeriod"`
	// startBackoff is the current delay between failed start attempts
	startBackoff time.Duration
	// backoff tracks the restart back-off across restarts, per replica.
	// Lazy-initialised by backoffStateOf() — never access this field directly.
	backoff *backoffState
	s       *State
}

// LogsEnabled reports whether stdout/stderr of the child process should be
// forwarded. When Logs is unset (nil) or explicitly true, logging is enabled;
// logs: false disables stdout/stderr forwarding for this command only.
func (c *Command) LogsEnabled() bool {
	return c.Logs == nil || *c.Logs
}

// Probe represents health check
// Can be http, tcp, file, env
// If health check is not represented default value is health success
type Probe struct {
	Period           Duration     `yaml:"period"`
	Delay            Duration     `yaml:"delay"`
	StartupDeadline  Duration     `yaml:"startupDeadline"`
	FailureThreshold int          `yaml:"failureThreshold"`
	Probes           []Health     `yaml:"-"`
	HTTP             *HTTPProbe   `yaml:"http"`
	TCP              *TCPProbe    `yaml:"tcp"`
	File             *FileProbe   `yaml:"file"`
	Env              *EnvProbe    `yaml:"env"`
	Exec             *ExecProbe   `yaml:"exec"`
	SQL              *SQLProbe    `yaml:"sql"`
	FloatingPid      *FloatingPid `yaml:"floatingPid"`
	HeartBeat        *HeartBeat   `yaml:"heartBeat"`
	AMQP             *AMQPProbe   `yaml:"amqp"`
	DNS              *DNSProbe    `yaml:"dns"`
	KeepSharedLock   *LockProbe   `yaml:"lock" json:"lock"`
}

// clone returns a copy of the probe config, so replicas don't share mutable
// probe state (DB/AMQP connections, defaults applied at runtime).
func (p *Probe) clone() *Probe {
	if p == nil {
		return nil
	}
	c := *p
	if p.SQL != nil {
		sql := *p.SQL
		sql.db = nil
		c.SQL = &sql
	}
	if p.AMQP != nil {
		amqp := *p.AMQP
		amqp.conn = nil
		c.AMQP = &amqp
	}
	if p.DNS != nil {
		dns := *p.DNS
		c.DNS = &dns
	}
	if p.FloatingPid != nil {
		fp := *p.FloatingPid
		c.FloatingPid = &fp
	}
	if p.HeartBeat != nil {
		hb := *p.HeartBeat
		c.HeartBeat = &hb
	}
	if p.KeepSharedLock != nil {
		l := *p.KeepSharedLock
		c.KeepSharedLock = &l
	}
	return &c
}

func (p *Probe) ProbesUndefined() bool {
	if p.TCP == nil &&
		p.HTTP == nil &&
		p.File == nil &&
		p.Env == nil &&
		p.Exec == nil &&
		p.SQL == nil &&
		p.FloatingPid == nil &&
		p.HeartBeat == nil &&
		p.AMQP == nil &&
		p.DNS == nil &&
		p.KeepSharedLock == nil {
		return true
	}
	return false
}

// NewSchedule starts processes represented in a config
// replica returns replica r of the command: a copy with its own name and
// runtime state (probes and hook are copied, not shared)
func (c *Command) replica(r int, s *State) *Command {
	return &Command{
		Name:                   ReplicaName(c.Name, r),
		Entrypoint:             c.Entrypoint,
		Args:                   c.Args,
		Envs:                   c.Envs,
		EnvsFile:               c.EnvsFile,
		RestartPolicy:          c.RestartPolicy,
		RestartBackoff:         c.RestartBackoff,
		Readiness:              c.Readiness.clone(),
		LogFormat:              c.LogFormat,
		Dir:                    c.Dir,
		PromTarget:             c.PromTarget,
		SysProcAttr:            c.SysProcAttr,
		PostStart:              c.PostStart.clone(),
		MemoryLimitMB:          c.MemoryLimitMB,
		TerminationGracePeriod: c.TerminationGracePeriod,
		IsJob:                  c.IsJob,
		Logs:                   c.Logs,
		LogLevels:              c.LogLevels,
		s:                      s,
	}
}

// ReplicaName returns the name of replica r of the command
func ReplicaName(name string, r int) string {
	return fmt.Sprintf("%v_%v", name, r)
}

func NewSchedule(ctx context.Context, commands []Command, skipCommands []string, sharedLockURL string, s *State, wg *sync.WaitGroup) {
	var commandList []string
	// Get process wrapper cpu/mem metrics
	pw := &Command{Name: "process-wrapper"}
	go pw.GetStats(ctx, os.Getpid())

	SharedLockURL = sharedLockURL

	schedule := func(commands []Command) {
		for i := 0; i < len(commands); i++ {
			if sliceContains(skipCommands, commands[i].Name) {
				log.Warnf("Skip %v command", commands[i].Name)
				continue
			}
			if commands[i].Replicas > 0 {
				for r := 0; r < commands[i].Replicas; r++ {
					replicaCommand := commands[i].replica(r, s)
					log.Debugf("Process replica command %+v", replicaCommand)
					commandList = append(commandList, replicaCommand.Name)
					// Not ready until probes pass, so /readiness doesn't report 200 before start
					s.SetState(replicaCommand.Name, false)
					wg.Add(1)
					go replicaCommand.Run(ctx, wg)
				}
			} else {
				commands[i].s = s
				log.Debugf("Process command %+v", commands[i])
				commandList = append(commandList, commands[i].Name)
				s.SetState(commands[i].Name, false)
				wg.Add(1)
				go commands[i].Run(ctx, wg)
			}
		}
	}

	schedule(commands)

	log.WithFields(log.Fields{"commands": fmt.Sprintf("%v", commandList)}).Printf("Wait all commands to complete")
	wg.Done()
}

func (c *Command) SetState(state *State) {
	c.s = state
}

// String prints command and args
func (c *Command) String() string {
	var cmd string
	cmd = c.Entrypoint
	for _, arg := range c.Args {
		cmd = fmt.Sprintf("%v %v", cmd, arg)
	}
	return cmd
}

func (c *Command) MergeEnvs() (map[string]string, error) {
	var err error
	envs := make(map[string]string, len(os.Environ())+len(c.Envs))
	logger := log.WithFields(log.Fields{"name": c.Name})

	// Load os envs
	for _, e := range os.Environ() {
		kv := parseEnv(e)
		if len(kv) == 2 {
			envs[kv[0]] = kv[1]
		} else {
			log.Warnf("Bad env: %v", e)
		}
	}

	// Load process envs
	for k, v := range c.Envs {
		envs[k] = v
	}

	// Load process envs from file
	if c.EnvsFile != "" {
		logger.Debugf("Load envs from file: %v", c.EnvsFile)
		var envsFile *os.File
		envsFile, err = os.Open(c.EnvsFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read envs file, %w", err)
		}

		fileScanner := bufio.NewScanner(envsFile)
		fileScanner.Split(bufio.ScanLines)

		for fileScanner.Scan() {
			text := fileScanner.Text()
			// Skip empty lines
			if len(text) == 0 {
				continue
			}
			kv := parseEnv(strings.TrimSpace(text))
			if len(kv) == 2 {
				envs[kv[0]] = removeQuotes(kv[1])
			} else {
				log.Errorf("Bad env %v", kv)
			}
		}
		err = envsFile.Close()
		if err != nil {
			logger.Errorf("Cannot close envs file, %v", err)
		}
	}
	logger.Debugf("Merged envs %+v", envs)
	return envs, nil
}

// ConvertEnvs converts envs to "key=value" form
func (c *Command) ConvertEnvs(envsMap map[string]string) []string {
	var envList []string
	for k, v := range envsMap {
		envList = append(envList, k+"="+v)
	}
	log.WithFields(log.Fields{"name": c.String()}).Debugf("Loaded envs: %v", envList)
	return envList
}

// Lease is the request body of the shared lock server lease endpoint
type Lease struct {
	Key     string            `json:"key"`
	Value   string            `json:"value"`
	Labels  map[string]string `json:"labels"`
	Created time.Time         `json:"timestamp"`
}

// CreateLease sends a POST request to create a lease and returns the lease ID
func (c *Command) CreateLease(ttlDuration string) (string, error) {
	var err error
	var data []byte
	url := SharedLockURL + DefaultSharedLockLeaseEndpoint

	lease := Lease{
		Key:   c.Name,
		Value: "",
		Labels: map[string]string{
			"command": c.String(),
			"hostname": func() string {
				var h string
				h, err = os.Hostname()
				if err != nil {
					log.Errorf("Cannot get OS hostname, %v", err)
					return ""
				}
				return h
			}(),
		},
		Created: time.Now().UTC(),
	}
	data, err = json.Marshal(lease)
	if err != nil {
		return "", fmt.Errorf("failed to marshal lease to json, %v", err)
	}

	buf := bytes.NewBuffer(data)
	// Create a new request using http.NewRequest
	req, err := http.NewRequest("POST", url, buf)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	// Set the Content-Type header (or any other headers)
	req.Header.Set("Content-Type", "text/plain")
	// Add more headers here as needed
	req.Header.Set("x-lease-ttl", ttlDuration)

	// Create a http.Client and send the request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create lease: %v", err)
	}
	defer resp.Body.Close()

	var body []byte
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	if resp.StatusCode == http.StatusCreated {
		return string(body), nil
	}

	return "", nil
}

func (c *Command) AcquireSharedLock(ctx context.Context) {
	preStartLockLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String(), "key": "/" + c.Name})
	if c.Readiness == nil || c.Readiness.KeepSharedLock == nil {
		preStartLockLogger.Debugf("Shared lock is disabled")
		return
	}
	var err error
	var lease string
	var ttlDuration time.Duration
	if c.Readiness.KeepSharedLock.TTLDuration == "" {
		c.Readiness.KeepSharedLock.TTLDuration = "2s"
	}
	c.s.RemoveState(c.Name)

	ttlDuration, err = time.ParseDuration(c.Readiness.KeepSharedLock.TTLDuration)
	if err != nil {
		preStartLockLogger.Warnf("Failed to parse lock.ttlDuration, %v", err)
		ttlDuration = time.Second * 2
	}

	once := sync.Once{}
	for {
		select {
		case <-ctx.Done():
			preStartLockLogger.Errorf("Command context was closed")
			return
		default:
			lease, err = c.CreateLease(c.Readiness.KeepSharedLock.TTLDuration)
			if err != nil {
				preStartLockLogger.Errorf("Failed to create a lease, %v", err)
				time.Sleep(ttlDuration)
				continue
			}

			if lease != "" {
				preStartLockLogger.Printf("Acquire a lease lock with ttl: %v", c.Readiness.KeepSharedLock.TTLDuration)
				c.Readiness.KeepSharedLock.Lease = lease
				metrics.SidecarAcquireLockTotal.WithLabelValues("success", c.Name, c.Entrypoint).Inc()
				return
			}
			once.Do(func() {
				preStartLockLogger.Warnf("Wait a lock")
			})
			time.Sleep(ttlDuration)
		}
	}
}

// keepSharedLock keeps the shared lock of a command that has finished for
// good (a completed job, restartPolicy: never) until shutdown: with the
// lease expired, another instance would take the lock and run it again
func (c *Command) keepSharedLock(ctx context.Context) {
	if c.Readiness == nil || c.Readiness.KeepSharedLock == nil || c.Readiness.KeepSharedLock.Lease == "" {
		return
	}
	period := c.Readiness.Period.Duration
	if period <= 0 {
		period = time.Second * ProbePeriodSeconds
	}
	log.WithFields(log.Fields{"name": c.Name}).Printf("Command finished, keep the shared lock until shutdown")
	for c.sleepUnlessShutdown(ctx, period) {
		c.Readiness.KeepSharedLock.Run(c.Name)
	}
}

// Run executes command with ctx cancel context and restarts it until shutdown.
// Each command runs unix signal handler and health check goroutines
func (c *Command) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for c.runOnce(ctx) {
	}
}

// runOnce starts the command, waits for it and reports whether it must be restarted
func (c *Command) runOnce(ctx context.Context) bool {
	startTime := time.Now().UTC()
	cmd := exec.CommandContext(ctx, c.Entrypoint, c.Args...) // #nosec G204
	preStartLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String()})
	envs, err := c.MergeEnvs()
	if err != nil {
		return c.startFailed(ctx, preStartLogger, err)
	}
	cmd.Env = c.ConvertEnvs(envs)
	if c.Dir != "" {
		preStartLogger.Printf("Set custom process dir to %v", c.Dir)
		cmd.Dir = c.Dir
	}

	// logs: false leaves stdout/stderr unset, os/exec connects them to os.DevNull
	logsEnabled := c.LogsEnabled()
	var stdoutReader, stderrReader *bufio.Reader
	if logsEnabled {
		var stdout, stderr io.ReadCloser
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return c.startFailed(ctx, preStartLogger, fmt.Errorf("cannot connect command stdout, %w", err))
		}
		defer stdout.Close()
		stdoutReader = bufio.NewReaderSize(stdout, MaxOutputLineBytes)
		stderr, err = cmd.StderrPipe()
		if err != nil {
			return c.startFailed(ctx, preStartLogger, fmt.Errorf("cannot connect command stderr, %w", err))
		}
		defer stderr.Close()
		stderrReader = bufio.NewReaderSize(stderr, MaxOutputLineBytes)
	} else {
		preStartLogger.Debugf("Logs disabled for command, stdout/stderr will not be forwarded")
	}

	// Own process group: stop signals reach the descendants of the command too
	cmd.SysProcAttr = groupProcAttr(c.SysProcAttr)
	cmd.Cancel = func() error { return SignalGroup(cmd.Process.Pid, syscall.SIGKILL) }

	// Don't take the shared lock for a command that can't start: the other
	// instances waiting for the lock would never get it while this one retries
	if err = checkStartable(cmd); err != nil {
		return c.startFailed(ctx, preStartLogger, err)
	}
	c.AcquireSharedLock(ctx)

	preStartLogger.Debugf("Schedule command")
	if err = cmd.Start(); err != nil {
		return c.startFailed(ctx, preStartLogger, err)
	}
	c.startBackoff = 0

	ctxRoutines, cancelRoutines := context.WithCancel(context.Background())
	pid := cmd.Process.Pid
	c.s.SetPid(c.Name, pid)
	c.s.SetCancelRoutine(c.Name, cancelRoutines)
	// Arm exactly one back-off reset for this incarnation. The first of
	// (a) readiness flipping to true or (b) uptime > SuccessThreshold
	// will fire it; subsequent triggers in the same run are no-ops.
	c.ArmRestartBackoffReset()
	postStartLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String(), "pid": pid})

	if logsEnabled {
		go c.ProcessOutput(stdoutReader, pid)
		go c.ProcessOutput(stderrReader, pid)
	}

	postStartLogger.Printf("Run command")

	// Run health checks
	healthDone := make(chan struct{})
	go func() {
		defer close(healthDone)
		c.HealthChecker(ctxRoutines, cancelRoutines, pid, envs)
	}()

	// Get process cpu/memory stats
	go c.GetStats(ctxRoutines, pid)

	postStartLogger.Debugf("Wait command to complete")

	err = cmd.Wait()
	exitCode := cmd.ProcessState.ExitCode()
	terminationLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String(), "pid": pid, "exit_code": exitCode})
	if err != nil {
		terminationLogger.Errorf("Command execution error, %v", err)
	}

	terminationLogger.Warnf("Command terminated")
	// Kill descendants left in the process group (background jobs), so they
	// don't outlive the command, e.g. after a restart or a lost shared lock
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		terminationLogger.Warnf("Killed processes left in the command process group")
	}
	c.s.RemovePid(c.Name, pid)

	metrics.SidecarCmdDurationSecondsGauge.WithLabelValues(c.Name, fmt.Sprintf("%v", exitCode)).Set(time.Since(startTime).Seconds())

	// restartPolicy: never — the command isn't restarted, even a job
	if normalizeRestartPolicy(c.RestartPolicy) == RestartPolicyNewer {
		terminationLogger.Warnf("Restart policy is 'never', do not restart")
		cancelRoutines()
		// The process won't come back: its readiness is final once the health
		// checker, which may still set it, has stopped. Exit code 0 is ready
		<-healthDone
		c.s.SetState(c.Name, exitCode == 0)
		go c.keepSharedLock(ctx)
		return false
	}

	// A job that exited by itself isn't restarted, a failed job stops the wrapper.
	// A job stopped by a graceful shutdown isn't a failure: the wrapper
	// finishes the shutdown of the other commands
	if c.IsJob && !c.s.IsKilled(c.Name) {
		cancelRoutines()
		// HANDLE_TERMINATION and a fast shutdown cancel ctx without the
		// shutdown state, os/exec then kills the job
		if c.s.IsShutdown() || ctx.Err() != nil {
			terminationLogger.Printf("Job stopped on shutdown, time %v", time.Since(startTime))
			return false
		}
		if exitCode != 0 {
			terminationLogger.Errorf("Job failed, time %v", time.Since(startTime))
			jobFailed(1)
		}
		// A completed job counts as ready (it must not keep the pod not ready,
		// and the readiness file waits for at least one ready process), once
		// the health checker, which may still set the state, has stopped
		<-healthDone
		c.s.SetState(c.Name, true)
		go c.keepSharedLock(ctx)
		terminationLogger.Printf("Job completed, time %v", time.Since(startTime))
		return false
	}

	// Do not restart process if command not killed by health checker and graceful shutdown in progress
	if !c.s.IsKilled(c.Name) && c.s.IsShutdown() {
		terminationLogger.Printf("Command completed with code %v, time %v",
			exitCode, time.Since(startTime))
		cancelRoutines()
		return false
	}

	// Restart command if exit code or HealthChecker routine/Memory limit handler killed the process
	if !c.s.IsShutdown() || c.s.IsKilled(c.Name) {
		c.s.SetKillState(c.Name, false)
		// Stop HealthChecker/GetStats before the back-off: it may be long.
		// Wait for the health checker: a probe still in flight must not reset
		// the back-off armed for the next run or share probe state with it.
		// A probe is bounded by its timeout and the process is already reaped
		cancelRoutines()
		<-healthDone

		if time.Since(startTime) >= c.ResolveBackoff().SuccessThreshold.Duration {
			c.ResetRestartBackoff("uptime")
		}
		delay := c.NextRestartDelay()
		failures := c.FailureCount()
		terminationLogger.WithFields(log.Fields{"backoff": delay.String(), "failures": failures}).
			Warnf("Restart command in %v (failures=%d)", delay, failures)
		metrics.SidecarCmdReadinessCount.WithLabelValues("restart", c.Name, c.Entrypoint).Inc()

		// SIGTERM during a long back-off must not stall graceful shutdown:
		// a graceful shutdown cancels ctx only after its deadline
		if !c.sleepUnlessShutdown(ctx, delay) {
			terminationLogger.Warnf("Shutdown during restart back-off, aborting restart")
			return false
		}
		return !c.s.IsShutdown()
	}
	cancelRoutines()
	return false
}

// jobFailed stops process-wrapper when a job fails, replaced in tests
var jobFailed = os.Exit

// PostStartHookRunner executes a hook command and sets the status to the state
func (c *Command) PostStartHookRunner(ctx context.Context, envs map[string]string) bool {
	s := c.PostStart.Run(ctx, c, envs)
	switch s {
	case true:
		c.s.SetState(c.Name, true)
	case false:
		c.s.SetState(c.Name, false)
	}
	return s
}

// SetReadinessDelay configures a probe startup delay
func (c *Command) SetReadinessDelay(logger *log.Entry) {
	if c.Readiness.Delay.Seconds() == 0 {
		c.Readiness.Delay.Duration = time.Second * ProbeDelaySeconds
		logger.Debugf("Set default readiness delay %v", c.Readiness.Delay)
	}
}

// SetReadinessPeriod configures a period between the probes
func (c *Command) SetReadinessPeriod(logger *log.Entry) {
	if c.Readiness.Period.Seconds() == 0 {
		c.Readiness.Period.Duration = time.Second * ProbePeriodSeconds
		logger.Debugf("Set default readiness period %v", c.Readiness.Period)
	}
}

// SetReadinessFailureThreshold configures a probe failure threshold
func (c *Command) SetReadinessFailureThreshold(logger *log.Entry) {
	if c.Readiness.FailureThreshold == 0 {
		c.Readiness.FailureThreshold = ProbeFailureThreshold
		logger.Debugf("Set default readiness failure threshold to %v", c.Readiness.FailureThreshold)
	}
}

// HealthChecker executes probes hooks and restarts unhealthy process
// If probes are undefined default status is healthy (HTTP 200)
// If one of the probes is failed global sidecar health status is unhealthy (HTTP 503)
// Global health state is tracked by *State.statusMap and *State.SetState method
func (c *Command) HealthChecker(ctxRoutines context.Context, cancelRoutines context.CancelFunc, pid int, envs map[string]string) {
	logger := log.WithFields(log.Fields{"name": c.Name, "pid": pid})
	logger.Debugf("Start health checker routine")
	if c.Readiness == nil {
		logger.Warnf("Readiness probe is not set for the process")
		if !c.PostStartHookRunner(ctxRoutines, envs) && ctxRoutines.Err() == nil {
			logger.Errorf("PostStart execution failure")
			c.killProcess(cancelRoutines, logger, pid)
		}
		return
	}
	c.SetReadinessDelay(logger)
	c.SetReadinessPeriod(logger)
	c.SetReadinessFailureThreshold(logger)
	// The DNS probe compares addresses with the ones at the process start:
	// the baseline is taken now, not after the readiness delay, so a change
	// during the delay restarts the process. If the lookup fails, the first
	// probe run takes the baseline
	if c.Readiness.DNS != nil {
		c.Readiness.DNS.reset()
		c.Readiness.DNS.Run(c.Name)
	}

	log.Debugf("Readiness Delay: %v", c.Readiness.Delay)
	log.Debugf("Readiness Period: %v", c.Readiness.Period)
	// Set default readiness state to false before first health run
	c.s.SetState(c.Name, false)

	if !sleepCtx(ctxRoutines, c.Readiness.Delay.Duration) {
		return
	}
	c.ProbeRunner(ctxRoutines, cancelRoutines, logger, pid, envs)
}

// ProbeRunner executes probes and collects a process state
func (c *Command) ProbeRunner(ctx context.Context, cancelRoutines context.CancelFunc, logger *log.Entry, pid int, envs map[string]string) {
	var (
		failureThreshold int
		hookExecuted     bool
	)

	for {
		var unhealthy int // Process probes failure counter
		select {
		// Handle routine termination
		case <-ctx.Done():
			logger.Warnf("Stop probe routine")
			return
		default:
			var status []bool
			if c.s.IsShutdown() {
				logger.Warnf("Stop probe routine, the process in shutdown state")
				return
			}
			if c.Readiness.HTTP != nil {
				s := HealthRun(c.Name, c.Readiness.HTTP)
				status = append(status, s)
			}
			if c.Readiness.TCP != nil {
				s := HealthRun(c.Name, c.Readiness.TCP)
				status = append(status, s)
			}
			if c.Readiness.File != nil {
				s := HealthRun(c.Name, c.Readiness.File)
				status = append(status, s)
			}
			if c.Readiness.Env != nil {
				s := HealthRun(c.Name, c.Readiness.Env)
				status = append(status, s)
			}
			if c.Readiness.Exec != nil {
				s := HealthRun(c.Name, c.Readiness.Exec.For(c))
				status = append(status, s)
			}
			if c.Readiness.SQL != nil {
				c.Readiness.SQL.Envs = envs
				s := HealthRun(c.Name, c.Readiness.SQL)
				status = append(status, s)
			}
			if c.Readiness.FloatingPid != nil {
				if c.Readiness.FloatingPid.State == nil {
					c.Readiness.FloatingPid.State = c.s
				}
				s := HealthRun(c.Name, c.Readiness.FloatingPid)
				status = append(status, s)
			}
			if c.Readiness.HeartBeat != nil {
				if c.Readiness.HeartBeat.State == nil {
					c.Readiness.HeartBeat.State = c.s
				}
				s := HealthRun(c.Name, c.Readiness.HeartBeat)
				status = append(status, s)
			}
			if c.Readiness.AMQP != nil {
				c.Readiness.AMQP.Envs = envs
				s := HealthRun(c.Name, c.Readiness.AMQP)
				status = append(status, s)
			}
			if c.Readiness.DNS != nil {
				s := HealthRun(c.Name, c.Readiness.DNS)
				status = append(status, s)
			}
			if c.Readiness.KeepSharedLock != nil {
				s := HealthRun(c.Name, c.Readiness.KeepSharedLock)
				status = append(status, s)
			}
			if c.Readiness.ProbesUndefined() {
				// Run PostStart hooks
				if !c.PostStartHookRunner(ctx, envs) && ctx.Err() == nil {
					logger.Errorf("PostStart execution failure")
					c.killProcess(cancelRoutines, logger, pid)
				}
				return
			}

			// Count failed probes, restart command if failure threshold of consecutive
			// failed checks is exceeded (the same semantics as kubelet probes)
			for _, s := range status {
				if s {
					metrics.SidecarCmdReadinessCount.WithLabelValues("success", c.Name, c.Entrypoint).Inc()
					continue
				}
				unhealthy++
				metrics.SidecarCmdReadinessCount.WithLabelValues("failed", c.Name, c.Entrypoint).Inc()
			}

			if unhealthy > 0 {
				c.s.SetState(c.Name, false)
				failureThreshold++
				logger.Warnf("Probe failure count %v, unhealthy probes %v", failureThreshold, unhealthy)
				if failureThreshold >= c.Readiness.FailureThreshold {
					logger.Warnf("Exceeded failure threshold, restart command, %v", c.Entrypoint)
					metrics.PushCommandRestartEvent(map[string]string{
						"command":      c.Entrypoint,
						"process_name": c.Name,
						"event_type":   "restart",
					})
					c.killProcess(cancelRoutines, logger, pid)
					if c.Readiness.KeepSharedLock != nil {
						c.s.RemoveState(c.Name)
					}
					return
				}
			} else {
				failureThreshold = 0
				// Run postStart hook once, after the first successful check
				if c.PostStart != nil && !hookExecuted {
					logger.Printf("Exec postStart hook")
					hookExecuted = true
					if !c.PostStartHookRunner(ctx, envs) {
						logger.Errorf("PostStart execution failure")
						c.killProcess(cancelRoutines, logger, pid)
						return
					}
				}
				c.s.SetState(c.Name, true)
				// The process became ready: the next crash starts from initialDelay
				c.ResetRestartBackoff("readiness")
			}
			logger.Debugf("Sleep before the next health checking action")
			if !sleepCtx(ctx, c.Readiness.Period.Duration) {
				logger.Warnf("Stop probe routine")
				return
			}
		}
	}
}

// killProcess marks the process as killed by the wrapper (so it is restarted) and terminates it
func (c *Command) killProcess(cancelRoutines context.CancelFunc, logger *log.Entry, pid int) {
	c.s.SetState(c.Name, false)
	c.s.SetKillState(c.Name, true)
	cancelRoutines()
	c.terminateProcess(logger, pid)
}

// terminateProcess sends SIGTERM and, if the process is still running after
// TerminationGracePeriod, SIGKILL. The process is considered exited once Run
// has reaped it and removed its pid from the state.
func (c *Command) terminateProcess(logger *log.Entry, pid int) {
	grace := c.TerminationGracePeriod.Duration
	if grace <= 0 {
		grace = DefaultTerminationGracePeriod
	}
	exited := func() bool { return c.s.GetPid(c.Name) != pid }

	logger.Warnf("Send SIGTERM, grace period %v", grace)
	if err := SignalGroup(pid, syscall.SIGTERM); err != nil {
		logger.Warnf("Terminate process failure %v", err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if exited() {
			return
		}
		time.Sleep(terminationPollInterval)
	}
	if exited() {
		return
	}
	logger.Warnf("Process is still running after %v, send SIGKILL", grace)
	if err := SignalGroup(pid, syscall.SIGKILL); err != nil {
		logger.Warnf("Kill process failure %v", err)
	}
}

// startFailed handles a command that can't be started (missing entrypoint,
// dir or envs file): instead of stopping the wrapper and all other commands,
// the command is marked unhealthy and retried with an exponential backoff.
func (c *Command) startFailed(ctx context.Context, logger *log.Entry, err error) bool {
	logger.Errorf("Start command error, %v", err)
	metrics.SidecarCmdReadinessCount.WithLabelValues("start_failure", c.Name, c.Entrypoint).Inc()
	c.s.SetState(c.Name, false)
	if c.s.IsShutdown() || ctx.Err() != nil {
		return false
	}
	c.startBackoff = min(max(c.startBackoff*2, StartRetryMinDelay), StartRetryMaxDelay)
	logger.Warnf("Retry command start in %v", c.startBackoff)
	return sleepCtx(ctx, c.startBackoff) && !c.s.IsShutdown()
}

// checkStartable reports the start failures that can be found without starting
// the command: an entrypoint that isn't in PATH or doesn't exist and a missing dir
func checkStartable(cmd *exec.Cmd) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	if cmd.Dir != "" {
		info, err := os.Stat(cmd.Dir)
		if err != nil {
			return fmt.Errorf("chdir %v: %w", cmd.Dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("chdir %v: not a directory", cmd.Dir)
		}
	}
	path := cmd.Path
	if !filepath.IsAbs(path) && cmd.Dir != "" {
		path = filepath.Join(cmd.Dir, path)
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return nil
}

// sleepCtx sleeps for d and returns false if ctx is done earlier
// sleepUnlessShutdown sleeps for d, it returns false early when ctx is
// cancelled or a graceful shutdown starts
func (c *Command) sleepUnlessShutdown(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if c.s.IsShutdown() {
			return false
		}
		left := time.Until(deadline)
		if left <= 0 {
			return true
		}
		if !sleepCtx(ctx, min(left, 100*time.Millisecond)) {
			return false
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ProcessOutput logs command output line by line. A line longer than
// MaxOutputLineBytes is logged in several parts, so a process can't make the
// wrapper buffer an unlimited amount of memory.
func (c *Command) ProcessOutput(b *bufio.Reader, pid int) {
	logFormat := os.Getenv("LOG_FORMAT")
	if c.LogFormat != "" {
		logFormat = c.LogFormat
	}
	for {
		out, err := readOutputLine(b)
		// Data may come together with an error (last line without a newline)
		if out != "" {
			c.logOutputLine(out, logFormat, pid)
		}
		if err != nil {
			log.WithFields(log.Fields{"name": c.Name}).Debugf("Stop process output routine")
			return
		}
	}
}

// readOutputLine returns the next line from b, or the next b.Size() bytes of
// it when the line doesn't fit into the reader buffer
func readOutputLine(b *bufio.Reader) (string, error) {
	line, err := b.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		err = nil
	}
	return string(line), err
}

func (c *Command) logOutputLine(out, logFormat string, pid int) {
	var level, message string

	getMessage := func(in string) (string, string) {
		d := logfmt.NewDecoder(strings.NewReader(out))
		var l, m string
		for d.ScanRecord() {
			if d.Err() != nil {
				log.Errorf("Scan record error, %v", d.Err())
			}
			for d.ScanKeyval() {
				if d.Err() != nil {
					log.Errorf("Scan key value error, %v", d.Err())
				}
				if string(d.Key()) == "level" {
					l = string(d.Value())
				}
				if string(d.Key()) == "msg" {
					m = string(d.Value())
				}
			}
		}
		if l == "" {
			return "unknown", in
		}
		return l, m
	}
	level, message = getMessage(out)

	// logLevels / LOG_LEVELS allow-list, an empty set disables the filter
	levels := effectiveLogLevels(c.LogLevels)

	if level == "unknown" {
		switch logFormat {
		case "free", "plain":
			fmt.Printf("%v", out)
		case "auto":
			emit, autoLevel := shouldEmit(levels, level, logFormat, out)
			if !emit {
				return
			}
			logger := log.WithFields(log.Fields{"name": c.Name, "pid": pid, "out": "stdout"})
			switch autoLevel {
			case LogLevelError:
				logger.Errorf("%v", out)
			case LogLevelWarning:
				logger.Warnf("%v", out)
			default:
				logger.Printf("%v", out)
			}
		default:
			log.WithFields(log.Fields{"name": c.Name, "pid": pid, "out": "stdout"}).Printf("%v", out)
		}
		return
	}

	if emit, _ := shouldEmit(levels, level, logFormat, out); !emit {
		return
	}
	logger := log.WithFields(log.Fields{"name": c.Name, "pid": pid, "severity": level})
	switch normalizeLevel(level) {
	case LogLevelWarning:
		logger.Warnf("%v", message)
	case LogLevelError:
		logger.Errorf("%v", message)
	case LogLevelDebug:
		logger.Debugf("%v", message)
	default:
		logger.Printf("%v", message)
	}
}

func removeQuotes(s string) string {
	if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseEnv convert env from key=value format to []string[key, value]
func parseEnv(e string) []string {
	for i := 0; i < len(e); i++ {
		if e[i] == '=' {
			return []string{e[0:i], e[i+1:]}
		}
	}
	return []string{}
}

func sliceContains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

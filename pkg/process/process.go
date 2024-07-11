package process

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/go-logfmt/logfmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"os"
	"os/exec"
	"process-wrapper/pkg/lease/handlers"
	"process-wrapper/pkg/metrics"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ProbePeriodSeconds                 = 5
	ProbeDelaySeconds                  = 5
	ProbeFailureThreshold              = 3
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
	Name          string               `yaml:"name"`
	Entrypoint    string               `yaml:"entrypoint"`
	Args          []string             `yaml:"args"`
	Envs          map[string]string    `yaml:"envs"`
	EnvsFile      string               `yaml:"envsFile"`
	RestartPolicy string               `yaml:"restartPolicy"` // Not implemented
	Readiness     *Probe               `yaml:"readiness"`
	LogFormat     string               `yaml:"logFormat"`
	Dir           string               `yaml:"dir"`
	PromTarget    string               `yaml:"promTarget"`
	SysProcAttr   *syscall.SysProcAttr `yaml:"sysProcAttr"`
	Replicas      int                  `yaml:"replicas"`
	PostStart     *Hook                `yaml:"postStart"`
	MemoryLimitMB float64              `yaml:"memoryLimitMB"`
	s             *State
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
	KeepSharedLock   *LockProbe   `yaml:"lock"json:"lock"`
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
		p.KeepSharedLock == nil {
		return true
	}
	return false
}

// NewSchedule starts processes represented in a config
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
					var replicaCommand *Command
					replicaCommandName := fmt.Sprintf("%v_%v", commands[i].Name, r)
					replicaCommand = &Command{
						Name:          replicaCommandName,
						Entrypoint:    commands[i].Entrypoint,
						Args:          commands[i].Args,
						Envs:          commands[i].Envs,
						EnvsFile:      commands[i].EnvsFile,
						Readiness:     commands[i].Readiness,
						LogFormat:     commands[i].LogFormat,
						SysProcAttr:   commands[i].SysProcAttr,
						MemoryLimitMB: commands[i].MemoryLimitMB,
						s:             s,
					}
					log.Debugf("Process replica command %+v", replicaCommand)
					commandList = append(commandList, replicaCommand.Name)
					wg.Add(1)
					go replicaCommand.Run(ctx, wg)
				}
			} else {
				commands[i].s = s
				log.Debugf("Process command %+v", commands[i])
				commandList = append(commandList, commands[i].Name)
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

func (c *Command) MergeEnvs() map[string]string {
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
			logger.Errorf("Cannot read envs file, %v", err)
			os.Exit(1)
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
	return envs
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

// CreateLease sends a POST request to create a lease and returns the lease ID
func (c *Command) CreateLease(ttlDuration string) (string, error) {
	var err error
	var data []byte
	url := SharedLockURL + DefaultSharedLockLeaseEndpoint

	lease := handlers.Lease{
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
	data, err = lease.ToJSON()
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
	client := &http.Client{}
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
				metrics.SidecarAcquireLockTotal.WithLabelValues("success", c.Name, c.String()).Inc()
				return
			}
			once.Do(func() {
				preStartLockLogger.Warnf("Wait a lock")
			})
			time.Sleep(ttlDuration)
		}
	}
}

// Run executes command with ctx cancel context
// Each command runs unix signal handler and health check goroutines
func (c *Command) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	startTime := time.Now().UTC()
	var err error
	cmd := exec.CommandContext(ctx, c.Entrypoint, c.Args...) // #nosec G204
	envs := c.MergeEnvs()
	cmd.Env = c.ConvertEnvs(envs)
	preStartLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String()})
	if c.Dir != "" {
		preStartLogger.Printf("Set custom process dir to %v", c.Dir)
		cmd.Dir = c.Dir
	}

	var stdout, stderr io.ReadCloser
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		preStartLogger.Errorf("Cannot connect command stdout, %v", err)
	}
	defer stdout.Close()
	stdoutReader := bufio.NewReader(stdout)
	stderr, err = cmd.StderrPipe()
	if err != nil {
		preStartLogger.Errorf("Cannot connect command stderr, %v", err)
	}
	defer stderr.Close()
	stderrReader := bufio.NewReader(stderr)

	if c.SysProcAttr != nil {
		log.Printf("Set process attributes")
		cmd.SysProcAttr = c.SysProcAttr
	}

	c.AcquireSharedLock(ctx)

	preStartLogger.Debugf("Schedule command")
	if err = cmd.Start(); err != nil {
		defer log.Fatalf("Start command error, %v", err)
		return
	}

	ctxRoutines, cancelRoutines := context.WithCancel(context.Background())
	pid := cmd.Process.Pid
	c.s.SetPid(c.Name, pid)
	c.s.SetCancelRoutine(c.Name, cancelRoutines)
	postStartLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String(), "pid": pid})

	go c.ProcessOutput(stdoutReader, pid)
	go c.ProcessOutput(stderrReader, pid)

	postStartLogger.Printf("Run command")

	// Run health checks
	go c.HealthChecker(ctxRoutines, cancelRoutines, pid, envs)

	// Get process cpu/memory stats
	go c.GetStats(ctxRoutines, pid)

	postStartLogger.Debugf("Wait command to complete")

	err = cmd.Wait()
	terminationLogger := log.WithFields(log.Fields{"name": c.Name, "command": c.String(), "pid": pid, "exit_code": cmd.ProcessState.ExitCode()})
	if err != nil {
		terminationLogger.Errorf("Command execution error, %v", err)
	}

	terminationLogger.Warnf("Command terminated")

	// Todo: Implement different restart policy for processes
	/*
		if c.RestartPolicy == RestartPolicyNewer {
			//terminationLogger.Warnf("DO NOT RESTART")
			cancelRoutines()
			return
		}

	*/
	metrics.SidecarCmdDurationSecondsGauge.WithLabelValues(c.Name, fmt.Sprintf("%v", cmd.ProcessState.ExitCode())).Set(time.Since(startTime).Seconds())

	// Do not restart process if command not killed by health checker and graceful shutdown in progress
	if !c.s.IsKilled(c.Name) && c.s.IsShutdown() {
		terminationLogger.Printf("Command completed with code %v, time %v",
			cmd.ProcessState.ExitCode(), time.Since(startTime))
		cancelRoutines()
		return
	}

	// Restart command if exit code or HealthChecker routine/Memory limit handler killed the process
	if !c.s.IsShutdown() || c.s.IsKilled(c.Name) {
		c.s.SetKillState(c.Name, false)
		terminationLogger.Warnf("Restart command")
		metrics.SidecarCmdReadinessCount.WithLabelValues("restart", c.Name, c.String()).Inc()
		time.Sleep(time.Second)
		cancelRoutines()
		if !c.s.IsShutdown() {
			wg.Add(1)
			defer c.Run(ctx, wg)
			return
		}
	}

}

// PostStartHookRunner executes a hook command and sets the status to the state
func (c *Command) PostStartHookRunner(ctx context.Context, envs map[string]string) bool {
	s := c.PostStart.Run(ctx, c.Name, envs)
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
	if c.Readiness.Delay.Duration.Seconds() == 0 {
		c.Readiness.Delay.Duration = time.Second * ProbeDelaySeconds
		logger.Debugf("Set default readiness delay %v", c.Readiness.Delay)
	}
}

// SetReadinessPeriod configures a period between the probes
func (c *Command) SetReadinessPeriod(logger *log.Entry) {
	if c.Readiness.Period.Duration.Seconds() == 0 {
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
		_ = c.PostStartHookRunner(ctxRoutines, envs)
		return
	}
	c.SetReadinessDelay(logger)
	c.SetReadinessPeriod(logger)
	c.SetReadinessFailureThreshold(logger)

	log.Debugf("Readiness Delay: %v", c.Readiness.Delay)
	log.Debugf("Readiness Period: %v", c.Readiness.Period)
	// Set default readiness state to false before first health run
	c.s.SetState(c.Name, false)

	time.Sleep(c.Readiness.Delay.Duration)
	c.ProbeRunner(ctxRoutines, cancelRoutines, logger, pid, envs)
}

// ProbeRunner executes probes and collects a process state
func (c *Command) ProbeRunner(ctx context.Context, cancelRoutines context.CancelFunc, logger *log.Entry, pid int, envs map[string]string) {
	var (
		err              error
		failureThreshold int
		hookExecuted     bool
		hookLastState    bool
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
				s := HealthRun(c.Name, c.Readiness.HTTP, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.TCP != nil {
				s := HealthRun(c.Name, c.Readiness.TCP, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.File != nil {
				s := HealthRun(c.Name, c.Readiness.File, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.Env != nil {
				s := HealthRun(c.Name, c.Readiness.Env, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.Exec != nil {
				s := HealthRun(c.Name, c.Readiness.Exec, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.SQL != nil {
				c.Readiness.SQL.Envs = envs
				s := HealthRun(c.Name, c.Readiness.SQL, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.FloatingPid != nil {
				if c.Readiness.FloatingPid.State == nil {
					c.Readiness.FloatingPid.State = c.s
				}
				s := HealthRun(c.Name, c.Readiness.FloatingPid, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.HeartBeat != nil {
				if c.Readiness.HeartBeat.State == nil {
					c.Readiness.HeartBeat.State = c.s
				}
				s := HealthRun(c.Name, c.Readiness.HeartBeat, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.AMQP != nil {
				c.Readiness.AMQP.Envs = envs
				s := HealthRun(c.Name, c.Readiness.AMQP, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.KeepSharedLock != nil {
				s := HealthRun(c.Name, c.Readiness.KeepSharedLock, c.s.M)
				status = append(status, s)
			}
			if c.Readiness.ProbesUndefined() {
				// Run PostStart hooks
				_ = c.PostStartHookRunner(ctx, envs)
				return
			}

			// Check probe statuses, restart command if failure threshold is exceeded
			for _, s := range status {
				if !s {
					unhealthy++
					metrics.SidecarCmdReadinessCount.WithLabelValues("failed", c.Name, c.String()).Inc()
					c.s.SetState(c.Name, false)
					failureThreshold++
					logger.Warnf("Probe failure count %v", failureThreshold)
					if failureThreshold >= c.Readiness.FailureThreshold {
						logger.Warnf("Execeded failure threshold, restart command, %v", c.String())
						c.s.M.PushCommandRestartEvent(map[string]string{
							"command":      c.String(),
							"process_name": c.Name,
							"event_type":   "restart",
						})
						c.s.SetKillState(c.Name, true)
						cancelRoutines()
						time.Sleep(time.Second * 2)
						err = syscall.Kill(pid, syscall.SIGKILL)
						if err != nil {
							logger.Warnf("Terminate process failure %v", err)
						}
						if c.Readiness.KeepSharedLock != nil {
							c.s.RemoveState(c.Name)
						}
						return
					}
				} else {
					metrics.SidecarCmdReadinessCount.WithLabelValues("success", c.Name, c.String()).Inc()
				}
			}
			// If one of the health checks is failed set process state to unhealthy
			if unhealthy > 0 {
				logger.Debugf("Unhealthy probes count %v", unhealthy)
				c.s.SetState(c.Name, false)
			}

			// If all health checks are successfully done, execute postStart hook and set state to healthy
			if unhealthy == 0 {
				// If hook isn't presented, set healthy state immediately
				if c.PostStart == nil {
					c.s.SetState(c.Name, true)
					unhealthy = 0
					failureThreshold = 0
				}
				if c.PostStart != nil && !hookExecuted {
					logger.Printf("Exec postStart hook")
					hookLastState = c.PostStartHookRunner(ctx, envs)
					hookExecuted = true
					if hookLastState {
						c.s.SetState(c.Name, true)
						unhealthy = 0
						failureThreshold = 0
					} else {
						logger.Errorf("PostStart execution failure")
						c.s.SetState(c.Name, false)
						c.s.SetKillState(c.Name, true)
						cancelRoutines()
						err = syscall.Kill(pid, syscall.SIGKILL)
						if err != nil {
							logger.Warnf("Terminate process failure %v", err)
						}
						return
					}
				}
			}
			logger.Debugf("Sleep before the next health checking action")
			time.Sleep(c.Readiness.Period.Duration)
		}
	}
}

// ProcessOutput prints commands output output
func (c *Command) ProcessOutput(b *bufio.Reader, pid int) {
	logFormat := os.Getenv("LOG_FORMAT")
	if c.LogFormat != "" {
		logFormat = c.LogFormat
	}
	for {
		out, err := b.ReadString('\n')
		if err != nil {
			log.WithFields(log.Fields{"name": c.Name}).Debugf("Stop process output routine")
			return
		}

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

		if level == "unknown" {
			switch logFormat {
			case "free", "plain":
				fmt.Printf("%v", out)
			case "auto":
				if strings.Contains(out, "CRITICAL") || strings.Contains(out, "ERROR") {
					log.WithFields(log.Fields{"name": c.Name, "pid": pid, "out": "stdout"}).Errorf("%v", out)
					continue
				}
				if strings.Contains(out, "WARNING") {
					log.WithFields(log.Fields{"name": c.Name, "pid": pid, "out": "stdout"}).Warnf("%v", out)
					continue
				}
				log.WithFields(log.Fields{"name": c.Name, "pid": pid, "out": "stdout"}).Printf("%v", out)
			default:
				log.WithFields(log.Fields{"name": c.Name, "pid": pid, "out": "stdout"}).Printf("%v", out)
			}
		} else {
			switch level {
			case "warning":
				log.WithFields(log.Fields{"name": c.Name, "pid": pid, "severity": level}).Warnf("%v", message)
			case "error":
				log.WithFields(log.Fields{"name": c.Name, "pid": pid, "severity": level}).Errorf("%v", message)
			default:
				log.WithFields(log.Fields{"name": c.Name, "pid": pid, "severity": level}).Printf("%v", message)
			}
		}
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

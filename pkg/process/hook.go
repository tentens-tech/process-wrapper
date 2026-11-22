package process

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	DefaultHookDeadlineSeconds = 30
	// MaxHookOutputBytes bounds how much of each command hook stream (stdout
	// and stderr separately) is kept. A hook that writes more is killed.
	MaxHookOutputBytes = 64 * 1024
)

var errHookOutputLimit = errors.New("hook output limit exceeded")

// cappedBuffer keeps at most limit bytes. The first write over the limit
// stores what still fits, calls onOverflow and fails, so exec stops copying.
type cappedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
	onOverflow func()
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.overflowed {
		return 0, errHookOutputLimit
	}
	room := b.limit - b.buf.Len()
	if len(p) <= room {
		return b.buf.Write(p)
	}
	n, _ := b.buf.Write(p[:room])
	b.overflowed = true
	b.onOverflow()
	return n, errHookOutputLimit
}

type Hook struct {
	SQL      *SQLHook          `yaml:"sql"`
	Command  *Command          `yaml:"command"`
	Envs     map[string]string `yaml:"envs"`
	Deadline Duration          `yaml:"deadline"`
}

// clone returns a copy of the hook for a replica: Run sets the default deadline
func (h *Hook) clone() *Hook {
	if h == nil {
		return nil
	}
	c := *h
	return &c
}

// Run executes the hook of the supervised command
func (h *Hook) Run(parent context.Context, supervised *Command, envs map[string]string) bool {
	if h == nil {
		return true
	}
	var state []bool
	name := supervised.Name

	if h.Deadline.Seconds() == 0 {
		h.Deadline.Duration = time.Second * DefaultHookDeadlineSeconds
	}
	ctx, cancel := context.WithDeadline(parent, time.Now().Add(h.Deadline.Duration))
	defer cancel()
	envs = overlayEnvs(envs, h.Envs)
	if h.SQL != nil {
		log.WithFields(log.Fields{"name": name}).Printf("Run sql hook")
		s := h.SQL.RunHook(ctx, name, envs)
		state = append(state, s)
	}
	if h.Command != nil {
		log.WithFields(log.Fields{"name": name}).Printf("Run command hook")
		s := h.Command.RunHook(ctx, supervised, envs)
		state = append(state, s)
	}
	for _, s := range state {
		if !s {
			return s
		}
	}
	return true
}

type SQLHook struct {
	Queries []string `yaml:"queries"`
}

func (h *SQLHook) RunHook(ctx context.Context, name string, envs map[string]string) bool {
	var err error
	var db *sql.DB
	dsn, ok := envs["MYSQL_DSN"]
	if !ok || dsn == "" {
		log.WithFields(log.Fields{"name": name}).Errorf("SQL hook execution error, MYSQL_DSN isn't set")
		return false
	}
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.WithFields(log.Fields{"name": name}).Errorf("SQL hook execution error, cannot connect to mysql, %v", err)
		return false
	}
	defer db.Close()

	for _, query := range h.Queries {
		_, err = db.ExecContext(ctx, query)
		if err != nil {
			log.Errorf("Cannot execute sql hook query: %v, %v", query, err)
			return false
		}
		log.WithFields(log.Fields{"name": name}).Printf("Successfully executed hook query: %v", query)
	}
	return true
}

// overlayEnvs returns a copy of envs with extra applied on top
func overlayEnvs(envs, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return envs
	}
	merged := make(map[string]string, len(envs)+len(extra))
	for k, v := range envs {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

func validateArgs(args []string) []string {
	var validated []string
	for i := 0; i < len(args); i++ {
		if args[i] == "" {
			continue
		}
		validated = append(validated, args[i])
	}
	return validated
}

// RunHook runs a command hook of the supervised command. The hook runs in
// its own process group (killed as a whole on deadline), with the
// supervised command credential and working directory (unless the hook sets
// its own dir), and its output is limited to MaxHookOutputBytes per stream.
func (c *Command) RunHook(ctx context.Context, supervised *Command, envs map[string]string) bool {
	name := supervised.Name
	logger := log.WithFields(log.Fields{"name": name, "hook": "command"})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := validateArgs(c.Args)
	cmd := exec.CommandContext(ctx, c.Entrypoint, args...) // #nosec G204
	cmd.Env = []string{}
	for k, v := range overlayEnvs(envs, c.Envs) {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%v=%v", k, v))
	}
	cmd.Dir = supervised.Dir
	if c.Dir != "" {
		cmd.Dir = c.Dir
	}
	cmd.SysProcAttr = childProcAttr(c.SysProcAttr, supervised.SysProcAttr)
	cmd.Cancel = func() error { return SignalGroup(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second

	stdout := &cappedBuffer{limit: MaxHookOutputBytes, onOverflow: cancel}
	stderr := &cappedBuffer{limit: MaxHookOutputBytes, onOverflow: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	// The process exited successfully, but its background jobs kept the
	// output open: not a failure, the jobs are killed below
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if cmd.Process != nil {
		// Don't leave background jobs of the hook running
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if o := stdout.buf.String(); o != "" {
		logger.Printf("%v", o)
	}
	if e := stderr.buf.String(); e != "" {
		logger.Warnf("%v", e)
	}
	if stdout.overflowed || stderr.overflowed {
		logger.Errorf("Command %v hook was killed, output exceeds %v bytes", c.String(), MaxHookOutputBytes)
		return false
	}
	if err != nil {
		logger.Errorf("Command %v hook executed with an error: %v", c.String(), err)
		return false
	}

	logger.Printf("Successfully executed command hook: `%v`", c.String())
	return true
}

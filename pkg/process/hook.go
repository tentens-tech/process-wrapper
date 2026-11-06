package process

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"os/exec"
	"syscall"
	"time"
)

const (
	DefaultHookDeadlineSeconds = 30
	// MaxHookOutputBytes bounds how much of each command hook stream (stdout
	// and stderr separately) the supervisor keeps. A hook that writes more is
	// killed and the rest of its output is discarded.
	MaxHookOutputBytes = 64 * 1024
)

var errHookOutputLimit = errors.New("hook output limit exceeded")

type Hook struct {
	SQL      *SQLHook          `yaml:"sql"`
	Command  *Command          `yaml:"command"`
	Envs     map[string]string `yaml:"envs"`
	Deadline Duration          `yaml:"deadline"`
}

func (h *Hook) Run(parent context.Context, name string, envs map[string]string) bool {
	if h == nil {
		return true
	}
	var state []bool

	if h.Deadline.Seconds() == 0 {
		h.Deadline.Duration = time.Second * DefaultHookDeadlineSeconds
	}
	ctx, cancel := context.WithDeadline(parent, time.Now().Add(h.Deadline.Duration))
	defer cancel()
	if h.SQL != nil {
		log.WithFields(log.Fields{"name": name}).Printf("Run sql hook")
		s := h.SQL.RunHook(ctx, name, envs)
		state = append(state, s)
	}
	if h.Command != nil {
		log.WithFields(log.Fields{"name": name}).Printf("Run command hook")
		s := h.Command.RunHook(ctx, name, envs)
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
		_, err = db.QueryContext(ctx, query)
		if err != nil {
			log.Errorf("Cannot execute sql hook query: %v, %v", query, err)
			return false
		}
		log.WithFields(log.Fields{"name": name}).Printf("Successfully executed hook query: %v", query)
	}
	return true
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

// cappedBuffer keeps at most limit bytes. The first write past the limit
// stores what still fits, calls onOverflow and fails, so exec stops copying
// and closes its end of the pipe.
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

// hookProcAttr returns a copy of attr that runs the hook in its own process
// group when the configuration allows it, and whether the hook's pid is also
// its process group id.
func hookProcAttr(attr *syscall.SysProcAttr) (*syscall.SysProcAttr, bool) {
	a := &syscall.SysProcAttr{}
	if attr != nil {
		copied := *attr
		a = &copied
	}
	if !a.Setsid && !a.Setpgid && !a.Foreground {
		a.Setpgid = true
		a.Pgid = 0
	}
	return a, a.Setsid || a.Pgid == 0
}

func (c *Command) RunHook(ctx context.Context, name string, envs map[string]string) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := validateArgs(c.Args)
	cmd := exec.CommandContext(ctx, c.Entrypoint, args...) // #nosec G204
	for k, v := range envs {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%v=%v", k, v))
	}
	if c.SysProcAttr != nil {
		log.WithFields(log.Fields{"name": name}).Printf("Set hook command process attributes")
	}
	var ownGroup bool
	cmd.SysProcAttr, ownGroup = hookProcAttr(c.SysProcAttr)
	cmd.Cancel = func() error {
		if ownGroup {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}

	stdout := &cappedBuffer{limit: MaxHookOutputBytes, onOverflow: cancel}
	stderr := &cappedBuffer{limit: MaxHookOutputBytes, onOverflow: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	logger := log.WithFields(log.Fields{"name": name, "hook": "command"})
	defer func() {
		if o := stdout.buf.String(); o != "" {
			logger.Printf("%v", o)
		}
		if r := stderr.buf.String(); r != "" {
			logger.Warnf("%v", r)
		}
		if stdout.overflowed {
			logger.Errorf("Command hook stdout exceeded %v bytes, the rest was discarded", MaxHookOutputBytes)
		}
		if stderr.overflowed {
			logger.Errorf("Command hook stderr exceeded %v bytes, the rest was discarded", MaxHookOutputBytes)
		}
	}()
	err := cmd.Run()
	if stdout.overflowed || stderr.overflowed {
		log.WithFields(log.Fields{"name": name}).Errorf("Command %v hook was killed, output limit of %v bytes exceeded", c.String(), MaxHookOutputBytes)
		return false
	}
	if err != nil {
		log.WithFields(log.Fields{"name": name}).Errorf("Command %v hook executed with an error: %v", c.String(), err)
		return false
	}

	log.WithFields(log.Fields{"name": name}).Printf("Successfully executed command hook: `%v`", c.String())
	return true
}

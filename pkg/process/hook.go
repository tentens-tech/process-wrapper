package process

import (
	"bytes"
	"context"
	"database/sql"
	log "github.com/sirupsen/logrus"
	"os/exec"
	"syscall"
	"time"
)

const (
	DefaultHookDeadlineSeconds = 30
)

type Hook struct {
	SQL      *SQLHook          `yaml:"sql"`
	Command  *Command          `yaml:"command"`
	Envs     map[string]string `yaml:"envs"`
	Deadline Duration          `yaml:"deadline"`
}

// Run executes the hooks. envs is used by in-process hooks only; a command hook
// gets h.Envs and its own envs, and runs with the identity of supervised.
func (h *Hook) Run(parent context.Context, name string, envs map[string]string, supervised *syscall.SysProcAttr) bool {
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
		s := h.Command.RunHook(ctx, name, h.Envs, supervised)
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

func (c *Command) RunHook(ctx context.Context, name string, hookEnvs map[string]string, supervised *syscall.SysProcAttr) bool {
	var stdout, stderr bytes.Buffer
	attr, err := childSysProcAttr(c.SysProcAttr, supervised)
	if err != nil {
		log.WithFields(log.Fields{"name": name}).Errorf("Command %v hook is not executed: %v", c.String(), err)
		return false
	}
	args := validateArgs(c.Args)
	cmd := exec.CommandContext(ctx, c.Entrypoint, args...) // #nosec G204
	cmd.Env = childEnv(hookEnvs, c.Envs)
	if attr != nil {
		log.WithFields(log.Fields{"name": name}).Printf("Set hook command process attributes")
		cmd.SysProcAttr = attr
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	defer func() {
		o := stdout.String()
		if o != "" {
			log.WithFields(log.Fields{"name": name, "hook": "command"}).Printf("%v", o)
		}
		r := stderr.String()
		if r != "" {
			log.WithFields(log.Fields{"name": name, "hook": "command"}).Warnf("%v", r)
		}
	}()
	err = cmd.Run()
	if err != nil {
		log.WithFields(log.Fields{"name": name}).Errorf("Command %v hook executed with an error: %v", c.String(), err)
		return false
	}

	log.WithFields(log.Fields{"name": name}).Printf("Successfully executed command hook: `%v`", c.String())
	return true
}

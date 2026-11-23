// Package phpscope runs the upstream `phpscope` binary (which in turn spawns
// `phpspy`) as a managed subprocess. It streams phpscope stdout/stderr into
// the process-wrapper logger, exposes a restart counter, and auto-restarts
// the process while the root context is alive.
package phpscope

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"process-wrapper/pkg/metrics"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	binaryPath     = "/usr/bin/phpscope"
	restartBackoff = 2 * time.Second
)

// Config drives the phpscope subprocess. Zero values are treated as "not set".
type Config struct {
	PyroscopeURL string
	AppName      string
	RateHz       int
}

// Enabled returns true if the integration is configured to run.
func (c Config) Enabled() bool {
	return c.PyroscopeURL != ""
}

// Run blocks until ctx is cancelled, (re)starting the phpscope subprocess on
// every non-clean exit with a fixed backoff. It is meant to be launched in a
// goroutine from main().
func Run(ctx context.Context, cfg Config) {
	if !cfg.Enabled() {
		return
	}

	appName := cfg.AppName
	if appName == "" {
		appName = defaultAppName()
	}
	rateHz := cfg.RateHz
	if rateHz <= 0 {
		rateHz = 400
	}

	logger := log.WithFields(log.Fields{"name": "phpscope", "app": appName})

	if _, err := os.Stat(binaryPath); err != nil {
		logger.Errorf("phpscope binary is missing at %v, PHP profiling disabled: %v", binaryPath, err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		args := []string{
			"--pyroscopeUrl=" + cfg.PyroscopeURL,
			"--appName=" + appName,
			"--rateHz=" + strconv.Itoa(rateHz),
		}

		cmd := exec.CommandContext(ctx, binaryPath, args...) // #nosec G204
		cmd.Env = os.Environ()

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			logger.Errorf("Cannot connect stdout: %v", err)
			metrics.SidecarPhpScopeRestartsTotal.WithLabelValues("pipe_error").Inc()
			waitRestart(ctx)
			continue
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			// Start isn't called, so os/exec doesn't close the stdout pipe
			_ = stdout.Close()
			logger.Errorf("Cannot connect stderr: %v", err)
			metrics.SidecarPhpScopeRestartsTotal.WithLabelValues("pipe_error").Inc()
			waitRestart(ctx)
			continue
		}

		logger.Printf("Start phpscope, url=%v, rate=%vHz", redactURL(cfg.PyroscopeURL), rateHz)
		if err := cmd.Start(); err != nil {
			logger.Errorf("Failed to start phpscope: %v", err)
			metrics.SidecarPhpScopeRestartsTotal.WithLabelValues("start_error").Inc()
			waitRestart(ctx)
			continue
		}

		go streamOutput(logger, stdout, "stdout")
		go streamOutput(logger, stderr, "stderr")

		err = cmd.Wait()
		if ctx.Err() != nil {
			logger.Printf("Context cancelled, stop phpscope runner")
			return
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		reason := "exit_" + strconv.Itoa(exitCode)
		if err != nil {
			logger.Warnf("phpscope terminated (exit=%v, err=%v), restart in %v", exitCode, err, restartBackoff)
		} else {
			logger.Warnf("phpscope terminated (exit=%v), restart in %v", exitCode, restartBackoff)
		}
		metrics.SidecarPhpScopeRestartsTotal.WithLabelValues(reason).Inc()
		waitRestart(ctx)
	}
}

func streamOutput(logger *log.Entry, r io.Reader, stream string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		logger.WithField("out", stream).Printf("%s", sc.Text())
	}
}

// defaultAppName builds a reasonable fallback app name so operators never
// ship profiles to Pyroscope without a name.
func defaultAppName() string {
	ns := os.Getenv("POD_NAMESPACE")
	host, _ := os.Hostname()
	if ns != "" && host != "" {
		return fmt.Sprintf("%s.%s", ns, host)
	}
	if host != "" {
		return host
	}
	return "process-wrapper.phpscope"
}

// waitRestart waits restartBackoff before the next phpscope start or until ctx is done
func waitRestart(ctx context.Context) {
	t := time.NewTimer(restartBackoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// redactURL hides the password of a URL with credentials, an unparsable URL
// isn't logged at all
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	return u.Redacted()
}

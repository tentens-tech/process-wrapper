package process

import (
	"encoding/json"
	"math/rand"
	"process-wrapper/pkg/metrics"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Defaults for the per-command exponential restart back-off. Chosen to be
// compatible with the previous fixed `time.Sleep(time.Second)` behaviour:
// the very first restart still waits ~1s, only repeated tight crash-loops
// escalate up to MaxDelay.
const (
	DefaultRestartBackoffInitialDelay     = time.Second
	DefaultRestartBackoffMaxDelay         = time.Minute
	DefaultRestartBackoffMultiplier       = 2.0
	DefaultRestartBackoffSuccessThreshold = 10 * time.Second
	DefaultRestartBackoffJitter           = 0.2
)

// RestartPolicyNever is a clearer alias for the historical, misspelled
// RestartPolicyNewer constant. Both expand to "never"; either name may be
// used in user-facing YAML and is normalised by normalizeRestartPolicy.
const RestartPolicyNever = RestartPolicyNewer

// RestartBackoff configures the exponential back-off used between
// automatic restarts of a command. All fields are optional; missing or
// zero values are filled by WithDefaults.
//
// Example YAML:
//
//	restartBackoff:
//	  initialDelay: 1s
//	  maxDelay: 1m
//	  multiplier: 2
//	  successThreshold: 10s
//	  jitter: 0.2
type RestartBackoff struct {
	// InitialDelay is the first delay applied after a process exits.
	// The next delay is InitialDelay * Multiplier, then *Multiplier
	// again, etc. — capped at MaxDelay.
	InitialDelay Duration `yaml:"initialDelay"`
	// MaxDelay caps the exponential growth.
	MaxDelay Duration `yaml:"maxDelay"`
	// Multiplier is the exponential growth factor; must be > 1, otherwise
	// the default (2) is used.
	Multiplier float64 `yaml:"multiplier"`
	// SuccessThreshold is the minimum uptime that counts as a "successful
	// start". When a process runs at least this long before exiting, the
	// failure counter is reset and the next delay falls back to
	// InitialDelay. The reset is hybrid: it can also be triggered earlier
	// by the readiness probe transitioning to healthy (see
	// (*Command).ResetRestartBackoff).
	SuccessThreshold Duration `yaml:"successThreshold"`
	// Jitter adds a uniform random component in the range
	// [-jitter, +jitter] (as a fraction of the delay) to avoid thundering
	// herd when many sidecars crash in lock-step. 0 disables jitter
	// (deterministic delays). Values are clamped to [0, 1].
	Jitter float64 `yaml:"jitter"`
}

// WithDefaults returns a copy of b with zero/invalid fields filled by
// package defaults. Safe on a nil receiver — that yields a fully
// defaulted configuration. The returned value is by-value to make callers
// resilient to in-place mutation by other goroutines.
func (b *RestartBackoff) WithDefaults() RestartBackoff {
	out := RestartBackoff{Jitter: DefaultRestartBackoffJitter}
	if b != nil {
		out = *b
	}
	if out.InitialDelay.Duration <= 0 {
		out.InitialDelay = Duration{Duration: DefaultRestartBackoffInitialDelay}
	}
	if out.MaxDelay.Duration <= 0 {
		out.MaxDelay = Duration{Duration: DefaultRestartBackoffMaxDelay}
	}
	if out.MaxDelay.Duration < out.InitialDelay.Duration {
		out.MaxDelay = out.InitialDelay
	}
	if out.Multiplier <= 1 {
		out.Multiplier = DefaultRestartBackoffMultiplier
	}
	if out.SuccessThreshold.Duration <= 0 {
		out.SuccessThreshold = Duration{Duration: DefaultRestartBackoffSuccessThreshold}
	}
	switch {
	case out.Jitter < 0:
		out.Jitter = DefaultRestartBackoffJitter
	case out.Jitter > 1:
		out.Jitter = 1
	}
	return out
}

// UnmarshalYAML applies the default jitter when the field is omitted, an
// explicit `jitter: 0` still disables it
func (b *RestartBackoff) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type plain RestartBackoff
	out := plain{Jitter: -1}
	if err := unmarshal(&out); err != nil {
		return err
	}
	*b = RestartBackoff(out)
	return nil
}

// UnmarshalJSON does the same as UnmarshalYAML for the JSON CONFIG env
func (b *RestartBackoff) UnmarshalJSON(data []byte) error {
	type plain RestartBackoff
	out := plain{Jitter: -1}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*b = RestartBackoff(out)
	return nil
}

// Restart delays (see nextBaseDelay and jitterDelay):
//   - prev <= 0 → start at cfg.InitialDelay (very first failure).
//   - otherwise → min(prev * Multiplier, MaxDelay), but never below
//     InitialDelay.
//   - the delay is jittered uniformly in [base * (1 - Jitter),
//     base * (1 + Jitter)] and capped at MaxDelay. Only the base is kept as
//     the next prev, so the jitter doesn't compound. Jitter == 0 disables it.

// nextBaseDelay is the exponential sequence without jitter: the jittered
// delay isn't fed back, so the jitter doesn't compound between restarts
func nextBaseDelay(prev time.Duration, cfg RestartBackoff) time.Duration {
	var base time.Duration
	if prev <= 0 {
		base = cfg.InitialDelay.Duration
	} else {
		base = time.Duration(float64(prev) * cfg.Multiplier)
		if base > cfg.MaxDelay.Duration {
			base = cfg.MaxDelay.Duration
		}
	}
	if base < cfg.InitialDelay.Duration {
		base = cfg.InitialDelay.Duration
	}
	return base
}

// jitterDelay applies the jitter to base, the result never exceeds MaxDelay
func jitterDelay(base time.Duration, cfg RestartBackoff, r *rand.Rand) time.Duration {
	if cfg.Jitter <= 0 {
		return base
	}
	// Float64 returns [0.0, 1.0); map to [-Jitter, +Jitter].
	delta := (r.Float64()*2 - 1) * cfg.Jitter
	out := time.Duration(float64(base) * (1 + delta))
	if out > cfg.MaxDelay.Duration {
		out = cfg.MaxDelay.Duration
	}
	if out < 0 {
		out = 0
	}
	return out
}

// backoffState tracks per-command back-off progression across restarts.
// All fields are guarded by mu. Each Command holds a *backoffState (not
// a value) so the embedded mutex is never accidentally copied when a
// Command is passed by value (ExecProbe.Command, log.Debugf("%+v"), …).
//
// Lifecycle within a single Run() incarnation:
//  1. Run() calls armReset() right after a successful cmd.Start().
//  2. ProbeRunner may call reset("readiness") when readiness flips to
//     true; the first call wins (resetArmed is set to false).
//  3. Run() calls reset("uptime") at termination if the process lived
//     long enough; if readiness already reset, this is a no-op.
//  4. Run() calls nextDelay() to compute the wait before the next
//     incarnation; this advances the failure counter.
type backoffState struct {
	mu         sync.Mutex
	rng        *rand.Rand
	current    time.Duration
	failures   int
	resetArmed bool
}

// backoffStateInit guards lazy initialisation of Command.backoff. Using
// a package-level mutex keeps Command itself copy-safe (no embedded
// mutex). The fast path is a nil-check that avoids the mutex hop after
// the first call per command.
var backoffStateInit sync.Mutex

// backoffStateOf returns the per-command back-off state, allocating it
// on first access. Safe for concurrent callers.
func (c *Command) backoffStateOf() *backoffState {
	if s := c.backoff; s != nil {
		return s
	}
	backoffStateInit.Lock()
	defer backoffStateInit.Unlock()
	if c.backoff == nil {
		c.backoff = &backoffState{}
	}
	return c.backoff
}

// armReset is called once per Run() right after a successful Start so
// that exactly one reset (uptime-based or readiness-based) can fire per
// process incarnation. Subsequent reset() calls within the same Run()
// after the first one are no-ops.
func (s *backoffState) armReset() {
	s.mu.Lock()
	s.resetArmed = true
	s.mu.Unlock()
}

// nextDelay computes the next delay, stores it as the new "previous"
// delay and bumps the failure counter. The reset arming flag is left
// alone: the new Run() that follows will call armReset() itself.
func (s *backoffState) nextDelay(cfg RestartBackoff) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rng == nil {
		// Lazy init — non-cryptographic, jitter only.
		s.rng = rand.New(rand.NewSource(time.Now().UnixNano())) // #nosec G404
	}
	s.current = nextBaseDelay(s.current, cfg)
	s.failures++
	return jitterDelay(s.current, cfg, s.rng)
}

// reset clears the counter only when armed; returns true when an actual
// reset happened. Disarms the flag in either case so a flapping
// readiness probe cannot reset more than once per process incarnation.
func (s *backoffState) reset() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resetArmed {
		return false
	}
	s.current = 0
	s.failures = 0
	s.resetArmed = false
	return true
}

// snapshot returns a stable view of the failure counter and the last
// delay — used for log fields only, not for control flow.
func (s *backoffState) snapshot() (failures int, current time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures, s.current
}

// ResolveBackoff returns the effective back-off configuration for the
// command (per-command override merged with package defaults).
func (c *Command) ResolveBackoff() RestartBackoff {
	return c.RestartBackoff.WithDefaults()
}

// NextRestartDelay computes the next delay before restarting the
// command, updates the Prometheus gauge and returns the delay. The
// returned value is always >= 0.
func (c *Command) NextRestartDelay() time.Duration {
	delay := c.backoffStateOf().nextDelay(c.ResolveBackoff())
	metrics.SidecarCmdRestartBackoffSeconds.WithLabelValues(c.Name).Set(delay.Seconds())
	return delay
}

// ResetRestartBackoff drops the consecutive-failures counter and
// reverts the next delay back to InitialDelay — but only when the
// reset has been armed for the current Run() incarnation. The reason
// argument is emitted as a debug-log field to help operators correlate
// resets with either uptime expiry or readiness transitions.
func (c *Command) ResetRestartBackoff(reason string) {
	if c.backoffStateOf().reset() {
		metrics.SidecarCmdRestartBackoffSeconds.WithLabelValues(c.Name).Set(0)
		log.WithFields(log.Fields{"name": c.Name, "reason": reason}).
			Debugf("Restart back-off counter reset")
	}
}

// ArmRestartBackoffReset arms the next reset for a fresh Run()
// incarnation. Must be called once after a successful cmd.Start().
func (c *Command) ArmRestartBackoffReset() {
	c.backoffStateOf().armReset()
}

// FailureCount returns the current consecutive failure count — exposed
// for log fields only; do not branch on it.
func (c *Command) FailureCount() int {
	f, _ := c.backoffStateOf().snapshot()
	return f
}

// normalizeRestartPolicy lowercases/trims the raw value and maps known
// aliases to canonical names. Unknown values are returned as-is so
// misconfiguration is visible in logs rather than silently coerced to a
// safe default.
func normalizeRestartPolicy(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "never", "newer":
		return RestartPolicyNewer
	case "", "always":
		return RestartPolicyAlways
	default:
		return s
	}
}

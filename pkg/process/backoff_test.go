package process

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// fixedCfg returns a deterministic config used by progression tests:
// 1s → 2x → 5m, no jitter.
func fixedCfg() RestartBackoff {
	return RestartBackoff{
		InitialDelay:     Duration{Duration: time.Second},
		MaxDelay:         Duration{Duration: 5 * time.Minute},
		Multiplier:       2,
		SuccessThreshold: Duration{Duration: 10 * time.Second},
		Jitter:           0,
	}
}

func TestRestartBackoffWithDefaultsZero(t *testing.T) {
	var b *RestartBackoff
	got := b.WithDefaults()
	if got.InitialDelay.Duration != DefaultRestartBackoffInitialDelay {
		t.Errorf("InitialDelay = %v, want %v", got.InitialDelay.Duration, DefaultRestartBackoffInitialDelay)
	}
	if got.MaxDelay.Duration != DefaultRestartBackoffMaxDelay {
		t.Errorf("MaxDelay = %v, want %v", got.MaxDelay.Duration, DefaultRestartBackoffMaxDelay)
	}
	if got.Multiplier != DefaultRestartBackoffMultiplier {
		t.Errorf("Multiplier = %v, want %v", got.Multiplier, DefaultRestartBackoffMultiplier)
	}
	if got.SuccessThreshold.Duration != DefaultRestartBackoffSuccessThreshold {
		t.Errorf("SuccessThreshold = %v, want %v", got.SuccessThreshold.Duration, DefaultRestartBackoffSuccessThreshold)
	}
	if got.Jitter != DefaultRestartBackoffJitter {
		t.Errorf("Jitter from a nil receiver = %v, want default %v", got.Jitter, DefaultRestartBackoffJitter)
	}
}

func TestRestartBackoffWithDefaultsPartial(t *testing.T) {
	b := &RestartBackoff{
		InitialDelay: Duration{Duration: 250 * time.Millisecond},
		Multiplier:   3,
		Jitter:       -1, // negative → defaulted
	}
	got := b.WithDefaults()
	if got.InitialDelay.Duration != 250*time.Millisecond {
		t.Errorf("InitialDelay overridden: got %v", got.InitialDelay.Duration)
	}
	if got.Multiplier != 3 {
		t.Errorf("Multiplier overridden: got %v", got.Multiplier)
	}
	if got.MaxDelay.Duration != DefaultRestartBackoffMaxDelay {
		t.Errorf("MaxDelay = %v, want default", got.MaxDelay.Duration)
	}
	if got.SuccessThreshold.Duration != DefaultRestartBackoffSuccessThreshold {
		t.Errorf("SuccessThreshold = %v, want default", got.SuccessThreshold.Duration)
	}
	if got.Jitter != DefaultRestartBackoffJitter {
		t.Errorf("Jitter = %v, want default %v", got.Jitter, DefaultRestartBackoffJitter)
	}
}

func TestRestartBackoffWithDefaultsClamps(t *testing.T) {
	b := &RestartBackoff{
		InitialDelay: Duration{Duration: 10 * time.Second},
		MaxDelay:     Duration{Duration: 1 * time.Second}, // less than initial
		Multiplier:   0.5,                                 // <= 1, must be replaced
		Jitter:       2.5,                                 // > 1, clamped to 1
	}
	got := b.WithDefaults()
	if got.MaxDelay.Duration != 10*time.Second {
		t.Errorf("MaxDelay should be raised to InitialDelay (%v), got %v", 10*time.Second, got.MaxDelay.Duration)
	}
	if got.Multiplier != DefaultRestartBackoffMultiplier {
		t.Errorf("Multiplier %v should be replaced by default %v", got.Multiplier, DefaultRestartBackoffMultiplier)
	}
	if got.Jitter != 1 {
		t.Errorf("Jitter clamp to 1, got %v", got.Jitter)
	}
}

func TestNextBackoffProgressionNoJitter(t *testing.T) {
	cfg := fixedCfg()
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		64 * time.Second,
		128 * time.Second,
		256 * time.Second,
		300 * time.Second, // capped (5m = 300s)
		300 * time.Second, // stays capped
		300 * time.Second,
	}
	var prev time.Duration
	for i, w := range want {
		prev = nextBackoff(prev, cfg, nil)
		if prev != w {
			t.Errorf("step %d: got %v, want %v", i, prev, w)
		}
	}
}

func TestNextBackoffJitterRange(t *testing.T) {
	cfg := fixedCfg()
	cfg.Jitter = 0.2
	r := rand.New(rand.NewSource(42))
	const iters = 1000
	for i := 0; i < iters; i++ {
		// Always start from 4s — fixed base, jitter only.
		got := nextBackoff(4*time.Second, cfg, r)
		// Expected base is min(4s * 2, 5m) = 8s; jittered ±20% → [6.4s, 9.6s].
		const base = 8 * time.Second
		low := time.Duration(float64(base) * 0.8)
		high := time.Duration(float64(base) * 1.2)
		if got < low || got > high {
			t.Fatalf("iter %d: %v outside [%v, %v]", i, got, low, high)
		}
	}
}

func TestNextBackoffJitterDeterministic(t *testing.T) {
	cfg := fixedCfg()
	cfg.Jitter = 0.2
	r1 := rand.New(rand.NewSource(7))
	r2 := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		a := nextBackoff(time.Second, cfg, r1)
		b := nextBackoff(time.Second, cfg, r2)
		if a != b {
			t.Fatalf("iter %d: same seed yielded different delays %v vs %v", i, a, b)
		}
	}
}

func TestNextBackoffStartsAtInitialDelay(t *testing.T) {
	cfg := fixedCfg()
	got := nextBackoff(0, cfg, nil)
	if got != cfg.InitialDelay.Duration {
		t.Errorf("first delay = %v, want %v", got, cfg.InitialDelay.Duration)
	}
	// Negative prev (defensive): same as zero.
	if got2 := nextBackoff(-time.Second, cfg, nil); got2 != cfg.InitialDelay.Duration {
		t.Errorf("negative prev gave %v, want %v", got2, cfg.InitialDelay.Duration)
	}
}

func TestBackoffStateProgressAndSnapshot(t *testing.T) {
	cfg := fixedCfg()
	var s backoffState
	for i := 1; i <= 5; i++ {
		s.nextDelay(cfg)
		f, c := s.snapshot()
		if f != i {
			t.Errorf("after %d failures, snapshot reports %d", i, f)
		}
		if c <= 0 {
			t.Errorf("current delay must be positive, got %v", c)
		}
	}
}

func TestBackoffStateArmingSemantics(t *testing.T) {
	cfg := fixedCfg()
	var s backoffState

	// Without arming: reset is a no-op.
	if s.reset() {
		t.Fatalf("reset on a non-armed state should return false")
	}

	// Arm then reset → succeeds, then second reset → no-op (disarmed).
	s.armReset()
	if !s.reset() {
		t.Fatalf("armed reset should succeed")
	}
	if s.reset() {
		t.Fatalf("second reset (disarmed) should be a no-op")
	}

	// Counter advances as expected, then re-arm + reset goes back to 0.
	s.nextDelay(cfg)
	s.nextDelay(cfg)
	if f, _ := s.snapshot(); f != 2 {
		t.Fatalf("expected 2 failures, got %d", f)
	}
	s.armReset()
	if !s.reset() {
		t.Fatalf("re-armed reset should succeed")
	}
	f, c := s.snapshot()
	if f != 0 || c != 0 {
		t.Fatalf("after reset want failures=0 current=0, got %d / %v", f, c)
	}

	// After reset, the next delay starts from InitialDelay again.
	got := s.nextDelay(cfg)
	if got != cfg.InitialDelay.Duration {
		t.Errorf("post-reset next delay = %v, want %v", got, cfg.InitialDelay.Duration)
	}
}

func TestBackoffStateConcurrentResetRace(t *testing.T) {
	// Even when many goroutines race to reset, exactly one wins per
	// arming. Smoke test for the mutex-guarded resetArmed flag.
	cfg := fixedCfg()
	var s backoffState
	const workers = 16

	for trial := 0; trial < 100; trial++ {
		s.nextDelay(cfg)
		s.armReset()

		results := make(chan bool, workers)
		for i := 0; i < workers; i++ {
			go func() { results <- s.reset() }()
		}
		var wins int
		for i := 0; i < workers; i++ {
			if <-results {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("trial %d: expected exactly one winner, got %d", trial, wins)
		}
	}
}

func TestNormalizeRestartPolicy(t *testing.T) {
	cases := map[string]string{
		"":         RestartPolicyAlways,
		"   ":      RestartPolicyAlways,
		"always":   RestartPolicyAlways,
		"ALWAYS":   RestartPolicyAlways,
		"never":    RestartPolicyNewer,
		" Never  ": RestartPolicyNewer,
		"NEVER":    RestartPolicyNewer,
		"newer":    RestartPolicyNewer, // historical typo accepted as alias
		"on-crash": "on-crash",         // unknown values preserved verbatim
	}
	for in, want := range cases {
		if got := normalizeRestartPolicy(in); got != want {
			t.Errorf("normalizeRestartPolicy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRestartPolicyConstantsAlias(t *testing.T) {
	// Both names must resolve to "never" — the public API exposes both
	// the historical typo and the cleaner alias.
	if RestartPolicyNewer != "never" || RestartPolicyNever != "never" {
		t.Fatalf("constants drifted: Newer=%q Never=%q", RestartPolicyNewer, RestartPolicyNever)
	}
}

// approxEqual is used by tests that exercise jitter math; we accept tiny
// float drift because rand.Float64 → time.Duration loses some precision.
func approxEqual(a, b time.Duration, eps float64) bool {
	if a == b {
		return true
	}
	return math.Abs(float64(a-b)) <= eps*float64(b+1)
}

func TestNextBackoffJitterDisabledExact(t *testing.T) {
	cfg := fixedCfg()
	cfg.Jitter = 0
	got := nextBackoff(2*time.Second, cfg, nil)
	want := 4 * time.Second
	if !approxEqual(got, want, 1e-9) {
		t.Errorf("jitter=0 should be exact: got %v, want %v", got, want)
	}
}

// The jitter doesn't compound between restarts and never exceeds maxDelay
func TestRestartBackoffJitterDoesNotCompound(t *testing.T) {
	cfg := RestartBackoff{
		InitialDelay: Duration{time.Second}, MaxDelay: Duration{8 * time.Second},
		Multiplier: 2, Jitter: 0.5,
	}
	for seed := int64(0); seed < 200; seed++ {
		s := &backoffState{rng: rand.New(rand.NewSource(seed))}
		base := time.Second
		for step := 0; step < 8; step++ {
			got := s.nextDelay(cfg)
			low := time.Duration(float64(base) * 0.5)
			high := time.Duration(float64(base) * 1.5)
			if high > cfg.MaxDelay.Duration {
				high = cfg.MaxDelay.Duration
			}
			if got < low || got > high {
				t.Fatalf("seed %d step %d: %v outside [%v, %v]", seed, step, got, low, high)
			}
			base *= 2
			if base > cfg.MaxDelay.Duration {
				base = cfg.MaxDelay.Duration
			}
		}
	}
}

// nextBackoff is the delay after prev, as the restart loop computes it
func nextBackoff(prev time.Duration, cfg RestartBackoff, r *rand.Rand) time.Duration {
	return jitterDelay(nextBaseDelay(prev, cfg), cfg, r)
}

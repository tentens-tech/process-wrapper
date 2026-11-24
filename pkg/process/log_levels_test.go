package process

import (
	"testing"
)

func TestNormalizeLevel(t *testing.T) {
	cases := map[string]string{
		"warn":        LogLevelWarning,
		"WARN":        LogLevelWarning,
		"  Warning ":  LogLevelWarning,
		"warning":     LogLevelWarning,
		"err":         LogLevelError,
		"ERROR":       LogLevelError,
		"dbg":         LogLevelDebug,
		"DEBUG":       LogLevelDebug,
		"trace":       LogLevelDebug,
		"info":        LogLevelInfo,
		"INFORMATION": LogLevelInfo,
		"notice":      LogLevelInfo,
		"":            LogLevelInfo,

		// High-severity aliases: must NOT collapse to info, otherwise a
		// child emitting level=fatal would silently disappear when the
		// filter allows only error.
		"fatal":     LogLevelError,
		"FATAL":     LogLevelError,
		"panic":     LogLevelError,
		"crit":      LogLevelError,
		"critical":  LogLevelError,
		"emerg":     LogLevelError,
		"emergency": LogLevelError,
		"alert":     LogLevelError,

		// Truly unknown values still fall back to info (documented).
		"foobar": LogLevelInfo,
	}
	for in, want := range cases {
		if got := normalizeLevel(in); got != want {
			t.Errorf("normalizeLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLogLevels(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // keys expected in set; nil means filter disabled
	}{
		{name: "empty", in: "", want: nil},
		{name: "whitespace only", in: "   ,  ,", want: nil},
		{name: "single", in: "warning", want: []string{"warning"}},
		{name: "multi with aliases", in: "warn, ERR, debug", want: []string{"warning", "error", "debug"}},
		{name: "padded", in: " warning , error ", want: []string{"warning", "error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseLogLevels(c.in)
			if c.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("size mismatch: got %v want %v", got, c.want)
			}
			for _, k := range c.want {
				if _, ok := got[k]; !ok {
					t.Errorf("missing key %q in %v", k, got)
				}
			}
		})
	}
}

func TestEffectiveLogLevelsFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string // nil = filter disabled
	}{
		{name: "unset", env: "", want: nil},
		{name: "warning+error", env: "warning,error", want: []string{"warning", "error"}},
		{name: "aliases", env: "warn, err", want: []string{"warning", "error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env == "" {
				t.Setenv(LogLevelsEnv, "")
			} else {
				t.Setenv(LogLevelsEnv, c.env)
			}
			got := effectiveLogLevels(nil)
			if c.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			for _, k := range c.want {
				if _, ok := got[k]; !ok {
					t.Errorf("missing %q in %v", k, got)
				}
			}
		})
	}
}

func TestEffectiveLogLevelsPrefersCommand(t *testing.T) {
	t.Setenv(LogLevelsEnv, "warning,error")

	got := effectiveLogLevels([]string{"debug"})
	if _, ok := got["debug"]; !ok {
		t.Fatalf("expected command-level debug, got %v", got)
	}
	if _, ok := got["warning"]; ok {
		t.Fatalf("env must not bleed in when command overrides: %v", got)
	}

	got = effectiveLogLevels(nil)
	if _, ok := got["warning"]; !ok {
		t.Fatalf("expected env fallback to include warning, got %v", got)
	}

	t.Setenv(LogLevelsEnv, "")
	if got := effectiveLogLevels(nil); got != nil {
		t.Fatalf("expected nil (filter disabled) when command and env empty, got %v", got)
	}
}

func TestLevelEnabled(t *testing.T) {
	// levelEnabled is on the hot path and assumes its input is already
	// normalized (callers go through normalizeLevel). These cases use
	// canonical lowercase values on purpose.
	if !levelEnabled(nil, LogLevelInfo) {
		t.Errorf("nil set must allow any level")
	}
	set := map[string]struct{}{LogLevelWarning: {}, LogLevelError: {}}
	if levelEnabled(set, LogLevelInfo) {
		t.Errorf("info must be filtered out")
	}
	if !levelEnabled(set, LogLevelWarning) {
		t.Errorf("warning must pass")
	}
	if !levelEnabled(set, LogLevelError) {
		t.Errorf("error must pass")
	}
	// Sanity: non-canonical input is intentionally not found — normalization
	// is the caller's responsibility.
	if levelEnabled(set, "WARN") {
		t.Errorf("non-normalized input must not match; callers must normalize first")
	}
}

func TestDetectAutoLevel(t *testing.T) {
	cases := map[string]string{
		"some CRITICAL failure":  LogLevelError,
		"app hit an ERROR state": LogLevelError,
		"FATAL: out of memory":   LogLevelError,
		"goroutine PANIC here":   LogLevelError,
		"WARNING: almost full":   LogLevelWarning,
		"everything is fine":     LogLevelInfo,
		"":                       LogLevelInfo,
	}
	for in, want := range cases {
		if got := detectAutoLevel(in); got != want {
			t.Errorf("detectAutoLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShouldEmit(t *testing.T) {
	// Allow only warning+error.
	set := parseLogLevels("warning,error")

	cases := []struct {
		name        string
		levels      map[string]struct{}
		parsedLevel string
		logFormat   string
		rawLine     string
		wantEmit    bool
		wantLevel   string
	}{
		{
			name:        "logfmt info dropped",
			levels:      set,
			parsedLevel: "info",
			logFormat:   "",
			rawLine:     "level=info msg=hello",
			wantEmit:    false,
			wantLevel:   LogLevelInfo,
		},
		{
			name:        "logfmt warning passes",
			levels:      set,
			parsedLevel: "warn",
			logFormat:   "",
			rawLine:     "level=warn msg=hey",
			wantEmit:    true,
			wantLevel:   LogLevelWarning,
		},
		{
			name:        "logfmt error passes",
			levels:      set,
			parsedLevel: "error",
			logFormat:   "",
			rawLine:     "level=error msg=boom",
			wantEmit:    true,
			wantLevel:   LogLevelError,
		},
		{
			// Regression guard: child emits level=fatal, the filter only
			// allows warning+error — the line MUST pass through (mapped to
			// error), not be silently dropped.
			name:        "logfmt fatal mapped to error",
			levels:      set,
			parsedLevel: "fatal",
			logFormat:   "",
			rawLine:     "level=fatal msg=oom",
			wantEmit:    true,
			wantLevel:   LogLevelError,
		},
		{
			name:        "logfmt panic mapped to error",
			levels:      set,
			parsedLevel: "panic",
			logFormat:   "",
			rawLine:     "level=panic msg=segfault",
			wantEmit:    true,
			wantLevel:   LogLevelError,
		},
		{
			// Truly unknown level still defaults to info → dropped when
			// filter excludes info. Documented and intentional.
			name:        "logfmt unknown level treated as info and dropped",
			levels:      set,
			parsedLevel: "foobar",
			logFormat:   "",
			rawLine:     "level=foobar msg=hi",
			wantEmit:    false,
			wantLevel:   LogLevelInfo,
		},
		{
			name:        "auto info dropped",
			levels:      set,
			parsedLevel: "unknown",
			logFormat:   "auto",
			rawLine:     "everything is fine",
			wantEmit:    false,
			wantLevel:   LogLevelInfo,
		},
		{
			name:        "auto error passes",
			levels:      set,
			parsedLevel: "unknown",
			logFormat:   "auto",
			rawLine:     "ERROR: boom",
			wantEmit:    true,
			wantLevel:   LogLevelError,
		},
		{
			name:        "free format never filtered",
			levels:      set,
			parsedLevel: "unknown",
			logFormat:   "free",
			rawLine:     "just a line",
			wantEmit:    true,
			wantLevel:   LogLevelInfo,
		},
		{
			name:        "plain format never filtered",
			levels:      set,
			parsedLevel: "unknown",
			logFormat:   "plain",
			rawLine:     "just a line",
			wantEmit:    true,
			wantLevel:   LogLevelInfo,
		},
		{
			name:        "default unknown passes through",
			levels:      set,
			parsedLevel: "unknown",
			logFormat:   "",
			rawLine:     "any line",
			wantEmit:    true,
			wantLevel:   LogLevelInfo,
		},
		{
			name:        "nil filter allows everything",
			levels:      nil,
			parsedLevel: "info",
			logFormat:   "",
			rawLine:     "level=info msg=hi",
			wantEmit:    true,
			wantLevel:   LogLevelInfo,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			emit, lvl := shouldEmit(c.levels, c.parsedLevel, c.logFormat, c.rawLine)
			if emit != c.wantEmit {
				t.Errorf("emit = %v, want %v", emit, c.wantEmit)
			}
			if lvl != c.wantLevel {
				t.Errorf("level = %q, want %q", lvl, c.wantLevel)
			}
		})
	}
}

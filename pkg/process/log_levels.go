package process

import (
	"os"
	"strings"
)

const (
	LogLevelsEnv = "LOG_LEVELS"

	LogLevelDebug   = "debug"
	LogLevelInfo    = "info"
	LogLevelWarning = "warning"
	LogLevelError   = "error"
)

// normalizeLevel lowercases, trims and maps common aliases to canonical
// names. High-severity aliases used by logrus/zerolog/slog/syslog
// (fatal/panic/critical/emergency/alert) are mapped to "error" so that a
// child emitting level=fatal is never silently dropped when the filter
// allows only error — this matches the behaviour of detectAutoLevel, which
// has always treated "CRITICAL" as error.
// Unknown values fall back to "info" (documented caveat).
func normalizeLevel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "warn", "warning":
		return LogLevelWarning
	case "err", "error",
		"fatal",
		"crit", "critical",
		"panic",
		"emerg", "emergency",
		"alert":
		return LogLevelError
	case "debug", "dbg", "trace":
		return LogLevelDebug
	case "", "info", "information", "notice":
		return LogLevelInfo
	default:
		return LogLevelInfo
	}
}

// buildLevelSet is the single place that turns a list of raw level tokens
// into a normalized set. Empty/whitespace tokens are skipped; if nothing
// remains the result is nil, which callers treat as "filter disabled —
// all levels allowed".
func buildLevelSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		set[normalizeLevel(t)] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// parseLogLevels converts a comma-separated list of levels into a set.
// Thin wrapper around buildLevelSet for the CSV shape (env var).
func parseLogLevels(raw string) map[string]struct{} {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return buildLevelSet(strings.Split(raw, ","))
}

// effectiveLogLevels resolves the active filter for a command. A non-empty
// per-command list wins over the global LOG_LEVELS env; when both are empty
// the filter is disabled (nil). The env is read on every call — this is
// cheap (called once per child-process stream) and keeps the function free
// of package-level state.
func effectiveLogLevels(cmdLevels []string) map[string]struct{} {
	if set := buildLevelSet(cmdLevels); set != nil {
		return set
	}
	return parseLogLevels(os.Getenv(LogLevelsEnv))
}

// levelEnabled returns true when the given canonical level passes the
// filter. A nil set means no filter is active, so every level is allowed.
// The level MUST already be normalized (see normalizeLevel); the check sits
// on the hot path of every emitted log line, so we skip re-normalization.
func levelEnabled(set map[string]struct{}, normalizedLevel string) bool {
	if set == nil {
		return true
	}
	_, ok := set[normalizedLevel]
	return ok
}

// detectAutoLevel mirrors the LOG_FORMAT=auto heuristic used by
// ProcessOutput: high-severity keywords (CRITICAL/ERROR/FATAL/PANIC) map to
// error, "WARNING" to warning, and everything else to info.
func detectAutoLevel(line string) string {
	if containsCaseInsensitive(line, "CRITICAL") ||
		containsCaseInsensitive(line, "ERROR") ||
		containsCaseInsensitive(line, "FATAL") ||
		containsCaseInsensitive(line, "PANIC") {
		return LogLevelError
	}
	if containsCaseInsensitive(line, "WARNING") {
		return LogLevelWarning
	}
	return LogLevelInfo
}

// shouldEmit resolves the severity for a given line and reports whether it
// should be forwarded. The filter is only applied when the line has a level
// that we can reasonably infer:
//   - logfmt: parsedLevel != "" && parsedLevel != "unknown"
//   - LOG_FORMAT=auto: severity is detected from keywords
//
// For LOG_FORMAT=free/plain and the default unknown-level path the filter is
// skipped to preserve backward compatibility — the caller still receives a
// best-effort level (used only for structured log fields).
func shouldEmit(levels map[string]struct{}, parsedLevel, logFormat, rawLine string) (emit bool, level string) {
	switch {
	case parsedLevel != "" && parsedLevel != "unknown":
		lvl := normalizeLevel(parsedLevel)
		return levelEnabled(levels, lvl), lvl
	case logFormat == "auto":
		lvl := detectAutoLevel(rawLine)
		return levelEnabled(levels, lvl), lvl
	default:
		return true, LogLevelInfo
	}
}

func containsCaseInsensitive(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

package phpscope

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://pyroscope:4040":             "http://pyroscope:4040",
		"https://user:secret@pyroscope:443": "https://user:xxxxx@pyroscope:443",
		"http://[::1%zz":                    "<invalid url>",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Children of phpscope (phpspy) are killed with it on shutdown
func TestRunKillsChildrenOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "phpscope")
	body := "#!/bin/sh\nsleep 300 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	old := binaryPath
	binaryPath = script
	t.Cleanup(func() { binaryPath = old })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Run(ctx, Config{PyroscopeURL: "http://pyroscope:4040"}); close(done) }()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("phpscope child didn't start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	deadline = time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the phpscope child outlived the runner")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

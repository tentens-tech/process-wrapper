// Probe helper executed inside an exec readiness probe.
// It records the identity and view of the filesystem the probe actually got.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		os.Stderr.WriteString("usage: probe_helper resultPath hostMarkerPath\n")
		os.Exit(2)
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		cwd = "error:" + cwdErr.Error()
	}
	marker := "hidden"
	if f, err := os.Open(os.Args[2]); err == nil {
		f.Close()
		marker = "visible"
	}
	env := "absent"
	if _, ok := os.LookupEnv("PROBE_SUPERVISOR_MARKER"); ok {
		env = "present"
	}
	body := fmt.Sprintf("uid=%d\ngid=%d\ncwd=%s\nmarker=%s\nenv=%s\n", os.Getuid(), os.Getgid(), cwd, marker, env)
	if err := os.WriteFile(os.Args[1], []byte(body), 0o644); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

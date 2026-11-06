package process

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestUnauthenticatedHealthEndpointsHideProcessTable is the spec for the
// unauthenticated process-table leak on the health listener.
//
// It fails while GET /pids, GET /status, GET /pidof, and GET /heartbeat return
// supervised names, pids, health bits, or heartbeat timestamps to a client that
// presents no credential (and to a client that presents only a placeholder
// Authorization value that was never issued). It passes once those responses
// no longer carry that table. Kubelet /readiness and /liveness stay on the
// same listener and must keep returning only an aggregate status code.
//
// The listener under test is State.HealthEndpoint, the same function NewState
// starts. NewState itself is not used here, so the test does not create the
// readiness file or start the preemption poller.
func TestUnauthenticatedHealthEndpointsHideProcessTable(t *testing.T) {
	t.Setenv("HEALTH_SOCKET", "")

	probe, err := net.Listen("tcp", "127.0.0.1:8111")
	if err != nil {
		t.Fatalf("default health port 8111 is not available to bind: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close port probe: %v", err)
	}

	const (
		readyName = "cmd-alpha"
		readyPid  = 35111
		downName  = "cmd-beta"
		downPid   = 35117
	)
	beat := time.Date(1999, 7, 14, 3, 4, 5, 0, time.UTC)

	s := &State{
		pidsMap:          map[string]int{},
		statusMap:        map[string]bool{},
		killedMap:        map[string]bool{},
		floatingPid:      map[string][]time.Time{},
		heartBeat:        map[string]time.Time{},
		cancelRoutineMap: map[string]context.CancelFunc{},
		preemptedEvent:   make(chan struct{}),
	}
	s.SetPid(readyName, readyPid)
	s.SetState(readyName, true)
	s.SetPid(downName, downPid)
	s.SetState(downName, false)
	s.SetHeartBeat(readyName, beat)

	go s.HealthEndpoint()

	if err := waitForListener("127.0.0.1:8111", 5*time.Second); err != nil {
		t.Fatalf("health listener did not accept connections: %v", err)
	}

	bases := reachableHealthBases(t)
	if len(bases) == 0 {
		t.Fatal("no address accepted connections on the default health port")
	}
	t.Logf("health listener accepted unauthenticated connections on %s", strings.Join(bases, ", "))

	// A value that was never issued. The listener must not treat it as
	// permission to read any supervised command.
	const placeholderAuthorization = "Bearer placeholder-not-a-credential"

	tableNeedles := []string{
		readyName,
		downName,
		strconv.Itoa(readyPid),
		strconv.Itoa(downPid),
	}

	for _, base := range bases {
		base := base
		t.Run(base, func(t *testing.T) {
			for _, header := range []string{"", placeholderAuthorization} {
				header := header
				label := "no-authorization"
				if header != "" {
					label = "placeholder-authorization"
				}
				t.Run(label, func(t *testing.T) {
					readiness := get(t, base+"/readiness", header)
					liveness := get(t, base+"/liveness", header)
					if readiness.status != http.StatusServiceUnavailable {
						t.Fatalf("GET /readiness status = %d, want 503 while %s is unhealthy; body %q", readiness.status, downName, readiness.body)
					}
					if liveness.status != http.StatusServiceUnavailable {
						t.Fatalf("GET /liveness status = %d, want 503 while %s is unhealthy; body %q", liveness.status, downName, liveness.body)
					}
					assertNoNeedles(t, "GET /readiness", readiness, tableNeedles)
					assertNoNeedles(t, "GET /liveness", liveness, tableNeedles)

					assertNoNeedles(t, "GET /pids", get(t, base+"/pids", header), tableNeedles)
					assertNoNeedles(t, "GET /status", get(t, base+"/status", header), tableNeedles)
					assertNoNeedles(t, "GET /pidof?name="+readyName, get(t, base+"/pidof?name="+readyName, header), []string{strconv.Itoa(readyPid), strconv.Itoa(downPid)})
					assertNoNeedles(t, "GET /pidof?name="+downName, get(t, base+"/pidof?name="+downName, header), []string{strconv.Itoa(readyPid), strconv.Itoa(downPid)})
					assertNoNeedles(t, "GET /heartbeat?name="+readyName, get(t, base+"/heartbeat?name="+readyName, header), []string{beat.String(), strconv.Itoa(readyPid)})
				})
			}
		})
	}
}

type healthResponse struct {
	status int
	body   string
}

func get(t *testing.T, rawURL, authorization string) healthResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", rawURL, err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
	return healthResponse{status: resp.StatusCode, body: string(body)}
}

func assertNoNeedles(t *testing.T, what string, resp healthResponse, needles []string) {
	t.Helper()
	var found []string
	for _, needle := range needles {
		if needle != "" && strings.Contains(resp.body, needle) {
			found = append(found, needle)
		}
	}
	if len(found) > 0 {
		t.Errorf("%s disclosed %q (status %d) body %q", what, found, resp.status, resp.body)
	}
}

func waitForListener(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return conn.Close()
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

// reachableHealthBases returns every local unicast address where the default
// health port accepts a TCP connection. Loopback covers a sibling container
// that shares the network namespace. Any other address shows the listener is
// reachable beyond localhost.
func reachableHealthBases(t *testing.T) []string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list interfaces: %v", err)
	}
	var bases []string
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			t.Fatalf("addrs for %s: %v", iface.Name, err)
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP
			if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			host := ip.String()
			if ip.To4() == nil {
				host = "[" + host + "]"
			}
			dialAddr := host + ":8111"
			conn, err := net.DialTimeout("tcp", dialAddr, 300*time.Millisecond)
			if err != nil {
				t.Logf("port 8111 not reachable on %s: %v", dialAddr, err)
				continue
			}
			conn.Close()
			base := "http://" + host + ":8111"
			if seen[base] {
				continue
			}
			seen[base] = true
			bases = append(bases, base)
		}
	}
	return bases
}

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"io"
	"net/http"
	"os"
	"process-wrapper/pkg/metrics"
	"process-wrapper/pkg/prommerge"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultMetadataURL   = "http://metadata.google.internal/computeMetadata/v1/instance/preempted?wait_for_change=true&timeout_sec=60"
	defaultEnvoyAdminURL = "http://localhost:15000/drain_listeners"
	defaultSocket        = ":8111"
	floatingPidHistory   = 10
	// Metadata server holds wait_for_change requests up to timeout_sec (60s)
	metadataClientTimeout = 90 * time.Second
	metadataRetryDelay    = 5 * time.Second
	waitPollInterval      = time.Second
)

// Health interface implements all probes
type Health interface {
	Run(string) bool
	String() string
}

func HealthRun(name string, h Health, m *metrics.Server) bool {
	s := h.Run(name)
	probeLabels := map[string]string{
		"event_type":   "readiness",
		"process_name": name,
	}
	probeLabels["probe"] = h.String()
	probeLabels["status"] = strconv.FormatBool(s)
	m.PushHealthEvent(probeLabels)

	log.Debugf("Probe: %v, status: %v", h.String(), s)
	return s
}

type State struct {
	Shutdown         bool
	Preempted        bool
	envoyDrain       bool
	ReadinessFile    string
	PreemptedCancel  context.CancelFunc
	killedMap        map[string]bool
	statusMap        map[string]bool
	pidsMap          map[string]int
	floatingPid      map[string][]time.Time
	heartBeat        map[string]time.Time
	cancelRoutineMap map[string]context.CancelFunc
	preemptedEvent   chan struct{}
	M                *metrics.Server
	mux              *http.ServeMux
	muxOnce          sync.Once
	sync.RWMutex
}

// Mux returns the handlers served on the health socket. It is a dedicated
// ServeMux instead of http.DefaultServeMux, so handlers that dependencies
// register globally (expvar /debug/vars, x/net/trace /debug/requests and
// /debug/events) are not exposed.
func (s *State) Mux() *http.ServeMux {
	s.muxOnce.Do(func() { s.mux = http.NewServeMux() })
	return s.mux
}

func NewState(readinessFile string, envoyDrain bool) *State {
	s := State{
		pidsMap:          make(map[string]int),
		statusMap:        make(map[string]bool),
		killedMap:        make(map[string]bool),
		floatingPid:      make(map[string][]time.Time),
		heartBeat:        make(map[string]time.Time),
		cancelRoutineMap: make(map[string]context.CancelFunc),
		preemptedEvent:   make(chan struct{}),
		envoyDrain:       envoyDrain,
		M:                metrics.NewMetricsServer(os.Getenv("METRICS_SERVER_ADDRESS")),
		ReadinessFile:    readinessFile,
	}
	go s.CreateReadinessFile()
	go s.HealthEndpoint()
	preemption := os.Getenv("HANDLE_PREEMPTION")
	switch preemption {
	case "true", "1", "enabled":
		var ctx context.Context
		ctx, s.PreemptedCancel = context.WithCancel(context.Background())
		go s.PreemptionChecker(ctx)
	}
	return &s
}

func (s *State) IsShutdown() bool {
	s.RLock()
	defer s.RUnlock()
	return s.Shutdown
}

func (s *State) SetShutdown() {
	s.Lock()
	defer s.Unlock()
	s.Shutdown = true
}

func (s *State) SetState(name string, status bool) {
	s.Lock()
	s.statusMap[name] = status
	s.Unlock()
}

func (s *State) RemoveState(name string) {
	s.Lock()
	delete(s.statusMap, name)
	s.Unlock()
}

func (s *State) SetHeartBeat(name string, t time.Time) {
	s.RLock()
	t0, ok := s.heartBeat[name]
	s.RUnlock()
	if ok {
		d := t.Sub(t0)
		metrics.SidecarCmdHeartBeatPeriodGauge.WithLabelValues(name).Set(d.Seconds())
		log.WithFields(log.Fields{"name": name}).Debugf("HearBeat period %v seconds", d.Seconds())
	}
	s.Lock()
	s.heartBeat[name] = t
	s.Unlock()
}

func (s *State) GetHeartBeat(name string) time.Time {
	s.RLock()
	defer s.RUnlock()
	return s.heartBeat[name]
}

func (s *State) GetAllHeartBeat() map[string]time.Time {
	s.RLock()
	defer s.RUnlock()
	heartBeat := make(map[string]time.Time, len(s.heartBeat))
	for k, v := range s.heartBeat {
		heartBeat[k] = v
	}
	return heartBeat
}

func (s *State) SetPid(name string, pid int) {
	s.Lock()
	s.pidsMap[name] = pid
	// Save process restart time
	s.floatingPid[name] = append(s.floatingPid[name], time.Now())
	// Prevent slice leak
	if len(s.floatingPid[name]) > floatingPidHistory {
		s.floatingPid[name] = s.floatingPid[name][len(s.floatingPid[name])-floatingPidHistory:]
	}
	s.Unlock()
}

// RemovePid forgets the pid of an exited process, so signals are never sent to a reused pid
func (s *State) RemovePid(name string, pid int) {
	s.Lock()
	if s.pidsMap[name] == pid {
		delete(s.pidsMap, name)
	}
	s.Unlock()
}

// IsKnownProcess reports whether a process with the name is managed by the wrapper
func (s *State) IsKnownProcess(name string) bool {
	s.RLock()
	defer s.RUnlock()
	_, ok := s.statusMap[name]
	if !ok {
		_, ok = s.pidsMap[name]
	}
	return ok
}

func (s *State) SetCancelRoutine(name string, cancel context.CancelFunc) {
	s.Lock()
	s.cancelRoutineMap[name] = cancel
	s.Unlock()
}

func (s *State) CancelRoutines() {
	s.RLock()
	for name, cancel := range s.cancelRoutineMap {
		log.WithFields(log.Fields{"name": name}).Warnf("Cancel process routine")
		cancel()
	}
	s.RUnlock()
}

func (s *State) NumOfRestarts(floatingPeriod time.Duration, name string) int {
	var r int
	s.RLock()
	data := s.floatingPid[name]
	s.RUnlock()
	log.WithFields(log.Fields{"name": name}).Debugf("Restart history: %v", data)
	for _, point := range data {
		if time.Now().Add(-floatingPeriod).Unix() < point.Unix() {
			r++
		}
	}
	return r
}

func (s *State) PrintState() {
	s.RLock()
	defer s.RUnlock()
	for name, status := range s.statusMap {
		fmt.Printf("Command: %v, pid: %v, ready: %v, terminating: %v\n", name, s.pidsMap[name], status, s.Shutdown)
	}
	fmt.Printf("Preempted: %v\n", s.Preempted)
}

func (s *State) GetPids() []int {
	var pids []int
	s.RLock()
	for _, pid := range s.pidsMap {
		pids = append(pids, pid)
	}
	defer s.RUnlock()
	return pids
}

func (s *State) GetPidsMap() map[string]int {
	s.RLock()
	pids := make(map[string]int, len(s.pidsMap))
	for k, v := range s.pidsMap {
		pids[k] = v
	}
	s.RUnlock()
	return pids
}

func (s *State) GetPid(name string) int {
	s.RLock()
	defer s.RUnlock()
	return s.pidsMap[name]
}

func (s *State) SetKillState(name string, status bool) {
	s.Lock()
	defer s.Unlock()
	s.killedMap[name] = status
}

func (s *State) IsKilled(name string) bool {
	s.RLock()
	defer s.RUnlock()
	return s.killedMap[name]
}

func (s *State) ProcessState() map[string]bool {
	statusMap := make(map[string]bool)
	s.RLock()
	for name, status := range s.statusMap {
		statusMap[name] = status
	}
	s.RUnlock()
	return statusMap
}

var metaHTTPClient *http.Client

func (s *State) GetMetadataClient() *http.Client {
	if metaHTTPClient == nil {
		metaHTTPClient = &http.Client{Timeout: metadataClientTimeout}
		return metaHTTPClient
	}
	return metaHTTPClient
}

var metadataRequest *http.Request

func (s *State) GetMetadataRequest() (*http.Request, error) {
	if metadataRequest == nil {
		var err error
		metadataURL := os.Getenv("METADATA_URL")
		if metadataURL == "" {
			metadataURL = defaultMetadataURL
		}
		// #nosec G704 -- URL comes from operator-controlled env (METADATA_URL) or a constant
		metadataRequest, err = http.NewRequest(http.MethodGet, metadataURL, nil)
		if err != nil {
			return nil, err
		}
		metadataRequest.Header.Set("Metadata-Flavor", "Google")
		return metadataRequest, nil
	}
	return metadataRequest, nil
}

func (s *State) WaitPreemptionState() {
	for {
		status, err := s.getPreemptionStatus()
		if err != nil {
			log.Warnf("Cannot get instance metadata to check node preemption, %v", err)
			time.Sleep(metadataRetryDelay)
			continue
		}
		if status == "TRUE" {
			s.preemptedEvent <- struct{}{}
			s.M.PushPreemptedEvent()
			log.Warnf("Node is preempted and terminated soon")
			return
		}
		log.Debugf("Preemption status: %v", status)
	}
}

func (s *State) getPreemptionStatus() (string, error) {
	req, err := s.GetMetadataRequest()
	if err != nil {
		return "", fmt.Errorf("build metadata request: %w", err)
	}
	resp, err := s.GetMetadataClient().Do(req) // #nosec G704 -- see GetMetadataRequest
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("read metadata response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata unexpected status code %v", resp.StatusCode)
	}
	return strings.TrimSpace(string(data)), nil
}

func (s *State) PreemptionChecker(ctx context.Context) {
	log.Printf("Start preemption checker")

	// Initialize preempted metric to make range vector promql functions work properly
	s.M.PushInitPreemptedMetric()
	go s.WaitPreemptionState()
	for {
		select {
		case <-ctx.Done():
			log.Printf("Cancel preemption checker")
			return
		case <-s.preemptedEvent:
			log.Warnf("Received preemption event")
			s.Lock()
			s.Preempted = true
			s.Unlock()
			s.PreemptedCancel()
			go s.DrainEnvoyListeners()
			log.Warnf("Stop preemption checker")
			return
		}
	}
}

func (s *State) DrainEnvoyListeners() {
	if !s.envoyDrain {
		return
	}
	log.Warnf("Drain envoy listeners")
	client := &http.Client{Timeout: 10 * time.Second}
	envoyAdminURL := os.Getenv("ENVOY_DRAIN_URL")
	if envoyAdminURL == "" {
		envoyAdminURL = defaultEnvoyAdminURL
	}

	var err error
	var envoyRequest *http.Request
	// #nosec G704 -- URL comes from operator-controlled env (ENVOY_DRAIN_URL) or a constant
	envoyRequest, err = http.NewRequest(http.MethodPost, envoyAdminURL, nil)
	if err != nil {
		log.Errorf("Envoy listeners drain error, cannot prepare request")
		return
	}
	var resp *http.Response
	resp, err = client.Do(envoyRequest) // #nosec G704 -- see above
	if err != nil {
		log.Errorf("Envoy listeners drain error, cannot make request, %v", err)
		return
	}
	defer resp.Body.Close()
	var data []byte
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Cannot read envoy response, %v", err)
		return
	}
	log.Printf("Envoy response %v", string(data))
}

func (s *State) IsPreempted() bool {
	s.RLock()
	defer s.RUnlock()
	return s.Preempted
}

func (s *State) CreateReadinessFile() {
	for {
		state := s.ProcessState()
		if len(state) == 0 {
			time.Sleep(time.Millisecond * 100)
			continue
		}
		var unhealthy int
		for _, r := range state {
			if !r {
				unhealthy++
			}
		}
		if unhealthy == 0 {
			err := os.WriteFile(s.ReadinessFile, []byte{}, 0600)
			if err != nil {
				log.Errorf("%v", err)
				time.Sleep(time.Second)
				continue
			}
			return
		}
		time.Sleep(time.Millisecond * 500)
	}
}

func (s *State) RemoveReadinessFile() {
	// The file may not exist if the processes never became ready
	err := os.Remove(s.ReadinessFile)
	if err != nil && !os.IsNotExist(err) {
		log.Errorf("%v", err)
	}
}

func (s *State) Readiness() bool {
	// Stop receiving traffic on preemption and during graceful shutdown
	if s.IsPreempted() || s.IsShutdown() {
		return false
	}
	for name, state := range s.ProcessState() {
		log.Debugf("Process %v, healthy state: %v", name, state)
		if !state {
			log.WithFields(log.Fields{"name": name}).Warnf("Appliction is degraded")
			return false
		}
	}
	return true
}

func (s *State) Liveness() bool {
	for name, state := range s.ProcessState() {
		log.Debugf("Process %v, healthy state: %v", name, state)
		if !state {
			log.WithFields(log.Fields{"name": name}).Warnf("Appliction is degraded")
			return false
		}
	}
	return true
}

// waitHealthy blocks until check returns true or the client goes away.
// The server-wide write timeout is lifted for this request.
func waitHealthy(w http.ResponseWriter, r *http.Request, check func() bool) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		log.Debugf("Cannot reset write deadline, %v", err)
	}
	t := time.NewTicker(waitPollInterval)
	defer t.Stop()
	for !check() {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *State) HealthEndpoint() {
	var socket string
	socket = os.Getenv("HEALTH_SOCKET")
	if socket == "" {
		socket = defaultSocket
	}
	mux := s.Mux()
	s.registerHealthHandlers(mux)
	server := &http.Server{
		Addr:         socket,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Fatalln(server.ListenAndServe())
}

// registerHealthHandlers registers readiness, liveness and process state endpoints
func (s *State) registerHealthHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/readiness", func(w http.ResponseWriter, r *http.Request) {
		log.Debugf("GET /readiness")
		if r.URL.Query().Has("wait") {
			waitHealthy(w, r, s.Readiness)
			return
		}
		health := s.Readiness()
		log.Debugf("Readiness status: %v", health)
		if !health {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
		log.Debugf("GET /liveness")
		if r.URL.Query().Has("wait") {
			waitHealthy(w, r, s.Liveness)
			return
		}
		health := s.Liveness()
		log.Debugf("Liveness status: %v", health)
		if !health {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/pidof", func(w http.ResponseWriter, r *http.Request) {
		log.Debugf("GET /pidof")
		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pid := s.GetPid(name)
		log.Debugf("PID of %v: %v", name, pid)
		if pid == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, err := fmt.Fprintf(w, "%d", pid)
		if err != nil {
			log.Errorf("Failed to write response for /pidof endpoint, %v", err)
		}
	})
	mux.HandleFunc("/pids", func(w http.ResponseWriter, _ *http.Request) {
		log.Debugf("GET /pids")
		pids := s.GetPidsMap()
		if len(pids) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		log.Debugf("PIDs map %+v", pids)
		jData, err := json.Marshal(pids)
		if err != nil {
			log.Errorf("Failed to marshal /pids body payload")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(jData)
		if err != nil {
			log.Errorf("Failed to write response for /pids endpoint, %v", err)
		}
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		log.Debugf("GET /status")
		state := s.ProcessState()
		if len(state) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		log.Debugf("Process state map %+v", state)
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(s.httpSerializer(state))
		if err != nil {
			log.Errorf("Failed to write response for /status endpoint, %v", err)
		}
	})

	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		log.Debugf("%v /heartbeat", r.Method)
		name := r.URL.Query().Get("name")
		// Only managed processes may report heartbeats: arbitrary names would grow
		// the state and the heartbeat metric without bound
		if name != "" && !s.IsKnownProcess(name) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		switch r.Method {
		case http.MethodGet:
			var body any = s.GetAllHeartBeat()
			if name != "" {
				body = map[string]string{"name": name, "lastHeartBeat": s.GetHeartBeat(name).String()}
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(s.httpSerializer(body)); err != nil {
				log.Errorf("Failed to write response for /heartbeat endpoint, %v", err)
			}
		case http.MethodPatch:
			if name == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.SetHeartBeat(name, time.Now())
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func (s *State) GetReadyPromTargets(targets []prommerge.Target) []prommerge.Target {
	var readyTargets []prommerge.Target
	ps := s.ProcessState()
	for i := range targets {
		if ps[targets[i].Name] {
			readyTargets = append(readyTargets, targets[i])
			continue
		}
		log.WithFields(log.Fields{"name": targets[i].Name}).Debugf("PromTarget is not ready")
	}
	return readyTargets
}

// PromMergeHandler registers /prommerge, which merges metrics of all ready processes
func (s *State) PromMergeHandler(targets []prommerge.Target, merger *prommerge.Merger) {
	s.Mux().HandleFunc("/prommerge", func(writer http.ResponseWriter, request *http.Request) {
		readyTargets := s.GetReadyPromTargets(targets)
		if len(readyTargets) == 0 {
			writer.WriteHeader(http.StatusNoContent)
			return
		}

		t := time.Now()
		families, err := merger.Collect(request.Context(), readyTargets)
		if err != nil {
			// Individual target failures are logged by the merger
			log.Debugf("Prommerge error, %v", err)
		}
		total := time.Since(t)
		if total > time.Second*30 {
			log.Printf("Slow promTargets collection: total=%v", total)
		}
		metrics.SidecarMetricsMergeDurationSecondsBucket.With(prometheus.Labels{}).Observe(total.Seconds())

		writer.Header().Set("Content-Type", prommerge.ContentType)
		if err = prommerge.Write(writer, families); err != nil {
			log.Errorf("Failed to write response for /prommerge endpoint, %v", err)
		}
	})
}

func (s *State) httpSerializer(in any) []byte {
	jData, err := json.Marshal(in)
	if err != nil {
		log.Errorf("Failed to marshal http body payload %+v, %v", in, err)
	}
	return jData
}

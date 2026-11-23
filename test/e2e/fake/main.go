// Command fake emulates the external services used by the e2e tests.
//
// HTTP on 127.0.0.1:18080:
//
//	GET  /preempted  GCE metadata "instance/preempted" (long poll, answers in 1s): FALSE or TRUE
//	POST /preempt    switch /preempted to TRUE
//	POST /drain      envoy admin drain_listeners
//	GET  /drains     number of /drain calls
//	POST /api/v1/import/prometheus  VMAgent import (preempted metric)
//	GET  /vmagent    lines received by the VMAgent import, one per line
//
// Shared lock server (API of github.com/tentens-tech/shared-lock), on
// 127.0.0.1:8191 (the default -shared-lock-url) and on 127.0.0.1:18080:
//
//	POST /lease      x-lease-ttl header, JSON {"key": ...}: 201 + lease ID, or 202 if the key is locked
//	POST /keepalive  lease ID: 200 if prolonged, 204 if the lease expired
//	GET  /health     200
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultLeaseTTL = 10 * time.Second

type lease struct {
	key     string
	ttl     time.Duration
	expires time.Time
}

type state struct {
	mu        sync.Mutex
	preempted bool
	drains    int
	vmagent   []string
	leases    map[string]*lease // by lease ID
	leaseID   int
}

// lockedBy returns the live lease of key
func (s *state) lockedBy(key string, now time.Time) *lease {
	for _, l := range s.leases {
		if l.key == key && now.Before(l.expires) {
			return l
		}
	}
	return nil
}

func (s *state) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /preempted", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor header", http.StatusForbidden)
			return
		}
		time.Sleep(time.Second)
		s.mu.Lock()
		preempted := s.preempted
		s.mu.Unlock()
		if preempted {
			fmt.Fprint(w, "TRUE")
		} else {
			fmt.Fprint(w, "FALSE")
		}
	})
	mux.HandleFunc("POST /preempt", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.preempted = true
		s.mu.Unlock()
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.drains++
		s.mu.Unlock()
		fmt.Fprint(w, "OK")
	})
	mux.HandleFunc("GET /drains", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		fmt.Fprint(w, s.drains)
	})
	mux.HandleFunc("POST /api/v1/import/prometheus", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.vmagent = append(s.vmagent, strings.TrimSpace(strings.ToValidUTF8(string(body), "�")))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /vmagent", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		fmt.Fprint(w, strings.Join(s.vmagent, "\n")+"\n")
	})
	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Key == "" {
			http.Error(w, "invalid lease", http.StatusBadRequest)
			return
		}
		ttl := defaultLeaseTTL
		if h := r.Header.Get("x-lease-ttl"); h != "" {
			d, err := time.ParseDuration(h)
			if err != nil || d <= 0 {
				http.Error(w, "invalid x-lease-ttl", http.StatusBadRequest)
				return
			}
			ttl = d
		}
		now := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lockedBy(req.Key, now) != nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.leaseID++
		id := strconv.Itoa(s.leaseID)
		s.leases[id] = &lease{key: req.Key, ttl: ttl, expires: now.Add(ttl)}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, id)
	})
	mux.HandleFunc("POST /keepalive", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		now := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		l, ok := s.leases[strings.TrimSpace(string(body))]
		if !ok || !now.Before(l.expires) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		l.expires = now.Add(l.ttl)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "OK")
	})
	return mux
}

func serve(addr string, h http.Handler) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func main() {
	s := &state{leases: make(map[string]*lease)}

	mux := s.routes()
	go serve("127.0.0.1:8191", mux)
	serve("127.0.0.1:18080", mux)
}

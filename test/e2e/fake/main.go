// Command fake emulates the external services used by the e2e tests.
//
// HTTP on 127.0.0.1:18080:
//
//	GET  /preempted  GCE metadata "instance/preempted" (long poll, answers in 1s): FALSE or TRUE
//	POST /preempt    switch /preempted to TRUE
//	POST /drain      envoy admin drain_listeners
//	GET  /drains     number of /drain calls
//	GET  /events     datagrams received by the metrics-server, one per line
//
// UDP on 127.0.0.1:18125: metrics-server events (JSON datagrams).
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type state struct {
	mu        sync.Mutex
	preempted bool
	drains    int
	events    []string
}

func (s *state) collect(conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			log.Fatalf("udp: %v", err)
		}
		s.mu.Lock()
		s.events = append(s.events, strings.ToValidUTF8(string(buf[:n]), "�"))
		s.mu.Unlock()
	}
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
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		fmt.Fprint(w, strings.Join(s.events, "\n")+"\n")
	})
	return mux
}

func main() {
	s := &state{}

	conn, err := net.ListenPacket("udp", "127.0.0.1:18125")
	if err != nil {
		log.Fatalf("udp: %v", err)
	}
	go s.collect(conn)

	srv := &http.Server{
		Addr:              "127.0.0.1:18080",
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

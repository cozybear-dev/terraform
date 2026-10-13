// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// stateServer is the least that Terraform's "http" backend needs: somewhere
// to GET, POST and DELETE a state, and to LOCK and UNLOCK it.
//
// It exists so that scenarios can keep their state somewhere other than a
// local file. That takes plan and apply through the code for remote state,
// where a snapshot is first written in memory and then persisted, which is
// the path a state in a bucket takes and which the fork changes: it decides
// whether a state has changed from a digest rather than by serializing it
// twice, and while applying it persists from the background.
type stateServer struct {
	listener net.Listener
	server   *http.Server

	mu       sync.Mutex
	state    []byte
	locked   bool
	requests []string
}

func newStateServer() (*stateServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &stateServer{listener: listener}
	s.server = &http.Server{Handler: s}
	go s.server.Serve(listener)
	return s, nil
}

func (s *stateServer) close() {
	s.server.Close()
}

// stateServerConfig is the configuration that selects the backend. Where
// the server is comes from the environment. See env.
const stateServerConfig = `terraform {
  backend "http" {}
}
`

// env returns the environment variables that tell the "http" backend where
// this server is.
func (s *stateServer) env() []string {
	address := "http://" + s.listener.Addr().String() + "/state"
	return []string{
		"TF_HTTP_ADDRESS=" + address,
		"TF_HTTP_LOCK_ADDRESS=" + address,
		"TF_HTTP_UNLOCK_ADDRESS=" + address,
	}
}

func (s *stateServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if s.state == nil {
			s.requests = append(s.requests, "GET, and there was no state")
			http.NotFound(w, r)
			return
		}
		s.requests = append(s.requests, "GET")
		w.Write(s.state)
	case http.MethodPost:
		// What matters about a write is that it happened and what it
		// wrote, which the comparison of the state itself covers, so only
		// the fact of it is recorded.
		s.requests = append(s.requests, "POST")
		s.state = body
	case http.MethodDelete:
		s.requests = append(s.requests, "DELETE")
		s.state = nil
	case "LOCK":
		if s.locked {
			s.requests = append(s.requests, "LOCK, refused")
			http.Error(w, "locked", http.StatusLocked)
			return
		}
		s.requests = append(s.requests, "LOCK")
		s.locked = true
	case "UNLOCK":
		s.requests = append(s.requests, "UNLOCK")
		s.locked = false
	default:
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}
}

// snapshot returns the state that's stored, and the requests received since
// the last call as sorted lines, each with how many times it was received.
func (s *stateServer) snapshot() (state, requests []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int)
	for _, request := range s.requests {
		counts[request]++
	}
	s.requests = nil
	lines := make([]string, 0, len(counts))
	for request, n := range counts {
		lines = append(lines, fmt.Sprintf("%s: %d", request, n))
	}
	sort.Strings(lines)
	state = s.state
	if state == nil {
		state = []byte("<absent>\n")
	}
	return state, []byte(strings.Join(lines, "\n") + "\n")
}

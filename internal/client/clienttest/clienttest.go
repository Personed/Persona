// Package clienttest serves the recorded contracts/api fixtures for client tests.
package clienttest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Personed/Persona/internal/contract"
)

// Server replays recorded exchanges. Each request to a path gets that path's
// next recorded response; the last one repeats.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	byPath   map[string][]contract.Exchange
	next     map[string]int
	requests []Request
}

// Request is a request the server received.
type Request struct {
	Path string
	Body json.RawMessage
}

// New serves every fixture in dir in recorded order.
func New(t testing.TB, dir string) *Server {
	t.Helper()
	fixtures, err := contract.Fixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all []contract.Exchange
	for _, name := range []string{"bots-conversation", "models-work", "native-conversation", "errors", "bot-name-validation"} {
		all = append(all, fixtures[name]...)
	}
	return Serve(t, all...)
}

// Serve replays exchanges.
func Serve(t testing.TB, exchanges ...contract.Exchange) *Server {
	t.Helper()
	s := &Server{byPath: map[string][]contract.Exchange{}, next: map[string]int{}}
	for _, e := range exchanges {
		s.byPath[e.Request.Path] = append(s.byPath[e.Request.Path], e)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Add appends a recorded exchange.
func (s *Server) Add(e contract.Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byPath[e.Request.Path] = append(s.byPath[e.Request.Path], e)
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var body json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{Path: r.URL.Path, Body: body})
	recorded := s.byPath[r.URL.Path]
	i := s.next[r.URL.Path]
	if i < len(recorded)-1 {
		s.next[r.URL.Path] = i + 1
	}
	s.mu.Unlock()
	if len(recorded) == 0 || r.Method != recorded[i].Request.Method {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"json":{"defined":false,"code":"NOT_FOUND","status":404,"message":"Not Found"}}`))
		return
	}
	res := recorded[i].Response
	if res.ContentType == "text/event-stream" {
		var stream string
		_ = json.Unmarshal(res.Body, &stream)
		w.Header().Set("Content-Type", res.ContentType)
		w.WriteHeader(res.Status)
		_, _ = w.Write([]byte(stream))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

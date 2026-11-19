package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
)

var (
	targetPath  = regexp.MustCompile(`^/([^/]+)/(status|start|stop|rebuild|daemon|process)$`)
	daemonToken = regexp.MustCompile(`^[a-f0-9]{32,128}$`)
)

// prepareError is the only failure detail clients see; logs and command output
// stay on the controller host.
const prepareError = `{"error":"电脑准备未完成。请重试。"}`

// Server is the authenticated control service of apps/controller. Every request
// needs "Authorization: Bearer <credential>". Routes:
//
//	GET  /health                     readiness, without touching Docker
//	GET  /<id>/status                {"state","network"}
//	POST /<id>/start|stop|rebuild    change, then {"state","network"}
//	CONNECT /<id>/daemon             pi-env on the tunnel; X-Persona-Daemon-Token
//	CONNECT /<id>/process            a Bot worker on the tunnel; X-Persona-Bot-Id
type Server struct {
	credential [sha256.Size]byte
	create     func(id string) (Driver, error)

	mu      sync.Mutex
	targets map[string]*Target
	closing bool
}

// NewServer serves driver's container and, when create is set, containers of other
// ids created on first use.
func NewServer(driver Driver, credential string, create func(id string) (Driver, error)) *Server {
	id := driver.Config().ID
	return &Server{
		credential: sha256.Sum256([]byte("Bearer " + credential)),
		create:     create,
		targets:    map[string]*Target{id: NewTarget(driver)},
	}
}

func (s *Server) trusted(r *http.Request) bool {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return subtle.ConstantTimeCompare(got[:], s.credential[:]) == 1 && !closing
}

func (s *Server) target(uri string) (*Target, string) {
	match := targetPath.FindStringSubmatch(uri)
	if match == nil || !ValidID(match[1]) {
		return nil, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.targets[match[1]]
	if target == nil && s.create != nil && !s.closing {
		driver, err := s.create(match[1])
		if err != nil {
			return nil, ""
		}
		target = NewTarget(driver)
		s.targets[match[1]] = target
	}
	if target == nil {
		return nil, ""
	}
	return target, match[2]
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !s.trusted(r) {
		reply(w, http.StatusUnauthorized, "{}")
		return
	}
	if r.Method == http.MethodGet && r.RequestURI == "/health" {
		reply(w, http.StatusOK, "{}")
		return
	}
	target, action := s.target(r.RequestURI)
	var status Status
	var err error
	// Lifecycle changes outlive a client that disconnects mid-way.
	ctx := context.WithoutCancel(r.Context())
	switch {
	case target != nil && r.Method == http.MethodGet && action == "status":
		status, err = target.Status(ctx)
	case target != nil && r.Method == http.MethodPost && (action == "start" || action == "stop" || action == "rebuild"):
		status, err = target.Change(ctx, Action(action))
	default:
		reply(w, http.StatusNotFound, "{}")
		return
	}
	if err != nil {
		reply(w, http.StatusServiceUnavailable, prepareError)
		return
	}
	data, _ := json.Marshal(status)
	reply(w, http.StatusOK, string(data))
}

func reply(w http.ResponseWriter, code int, body string) {
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Persona-Daemon-Token")
	botID, botErr := strconv.Atoi(r.Header.Get("X-Persona-Bot-Id"))
	var target *Target
	var action string
	if s.trusted(r) {
		target, action = s.target(r.RequestURI)
	}
	allowed := target != nil && (action == "process" && botErr == nil && botID >= 1 ||
		action == "daemon" && daemonToken.MatchString(token))
	if !allowed {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if action == "process" {
		token = ""
	} else {
		botID = 0
	}
	channel, err := target.Prepare(context.WithoutCancel(r.Context()), token, botID)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	process, err := channel.Start(conn)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\n\r\n")
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		process.Kill()
	}
	go func() {
		// Bytes the client sent after the CONNECT head are already buffered.
		_, _ = io.Copy(process.Stdin, buffered)
		_ = process.Stdin.Close()
		// The client closed its side: end the channel.
		process.Kill()
	}()
	<-process.Done()
	closeWrite(conn)
}

func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = tcp.CloseWrite()
	}
}

// Close refuses new requests, ends every channel and stops the containers this
// controller used.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	targets := make([]*Target, 0, len(s.targets))
	for _, target := range s.targets {
		targets = append(targets, target)
	}
	s.mu.Unlock()
	errs := make(chan error, len(targets))
	for _, target := range targets {
		go func() { errs <- target.Close(ctx) }()
	}
	var first error
	for range targets {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

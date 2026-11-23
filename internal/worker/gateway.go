package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/mcpserver"
)

const gatewayChunkBytes = 64 << 10

// gatewaySlots bounds in-flight requests on each side of the relay.
const gatewaySlots = 32

// Gateway exposes the service's product-tool gateway on the worker loopback.
// It never exposes other service endpoints or enables SSH port forwarding.
type Gateway struct {
	URL    string
	owner  *Client
	cancel context.CancelFunc
	done   chan struct{}
}

// Close stops the worker endpoint and cancels its forwarded requests.
func (g *Gateway) Close() error {
	g.cancel()
	<-g.done
	return nil
}

type gatewayEvent struct {
	Type   string      `json:"type"`
	URL    string      `json:"url,omitempty"`
	ID     uint32      `json:"id,omitempty"`
	Method string      `json:"method,omitempty"`
	Header http.Header `json:"header,omitempty"`
}

type gatewayReply struct {
	Stream uint32      `json:"stream"`
	ID     uint32      `json:"id"`
	Status int         `json:"status,omitempty"`
	Header http.Header `json:"header,omitempty"`
	End    bool        `json:"end,omitempty"`
	Failed bool        `json:"failed,omitempty"`
	data   []byte
}

type gatewayRequest struct {
	ctx     context.Context
	replies chan gatewayReply
}

type gatewayStream struct {
	mu      sync.Mutex
	next    uint32
	pending map[uint32]gatewayRequest
}

func (s *server) serveGateway(ctx context.Context, id uint32, stream *gatewayStream) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	path := "/mcp/" + token
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		stream.handle(w, r, func(event gatewayEvent, payload []byte) {
			s.send(frameEvent, id, event, payload)
		})
	})
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	s.send(frameEvent, id, gatewayEvent{Type: "ready", URL: "http://" + listener.Addr().String() + path}, nil)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *gatewayStream) handle(w http.ResponseWriter, r *http.Request, send func(gatewayEvent, []byte)) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	if len(s.pending) >= gatewaySlots {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.next++
	id := s.next
	request := gatewayRequest{ctx: r.Context(), replies: make(chan gatewayReply)}
	s.pending[id] = request
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpserver.MaxRequestBytes))
	if err != nil {
		http.Error(w, "Could not read MCP request", http.StatusBadRequest)
		return
	}
	send(gatewayEvent{Type: "http", ID: id, Method: r.Method, Header: requestHeaders(r.Header)}, body)
	defer send(gatewayEvent{Type: "cancel", ID: id}, nil)
	controller := http.NewResponseController(w)
	sentHeader := false
	for {
		select {
		case <-r.Context().Done():
			return
		case reply := <-request.replies:
			if reply.Failed {
				if sentHeader {
					panic(http.ErrAbortHandler)
				}
				http.Error(w, "Persona tool gateway is unavailable", http.StatusBadGateway)
				return
			}
			if !sentHeader && reply.Status >= 200 && reply.Status <= 599 {
				for key, values := range reply.Header {
					w.Header()[key] = values
				}
				w.WriteHeader(reply.Status)
				sentHeader = true
			}
			if len(reply.data) > 0 {
				if _, err := w.Write(reply.data); err != nil {
					return
				}
			}
			if reply.End {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *server) replyGateway(f frame) error {
	reply, err := decode[gatewayReply](f.JSON)
	if err != nil {
		return err
	}
	s.mu.Lock()
	r := s.requests[reply.Stream]
	s.mu.Unlock()
	if r == nil || r.gateway == nil || len(f.Payload) > gatewayChunkBytes {
		return codeError("EINVAL", "invalid gateway reply", "")
	}
	stream := r.gateway
	stream.mu.Lock()
	request, ok := stream.pending[reply.ID]
	stream.mu.Unlock()
	if !ok {
		return codeError("ENOENT", "gateway request ended", "")
	}
	reply.data = f.Payload
	select {
	case request.replies <- reply:
		return nil
	case <-request.ctx.Done():
		return request.ctx.Err()
	}
}

// OpenGateway relays the task's G3 gateway over this worker connection. Pass
// the returned URL to Pi as PERSONA_PI_MCP_URL, not the service's loopback URL.
func (c *Client) OpenGateway(ctx context.Context, service *mcpserver.Gateway) (*Gateway, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if service == nil {
		return nil, errors.New("worker: product gateway is required")
	}
	target, err := url.Parse(service.URL)
	if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" || target.Port() == "" || target.User != nil {
		return nil, errors.New("worker: product gateway must be on service loopback")
	}
	lifetime, cancel := context.WithCancel(ctx)
	ready := make(chan string, 1)
	started := make(chan struct{})
	var streamID uint32
	var mu sync.Mutex
	active := map[uint32]context.CancelFunc{}
	id, call, err := c.begin("gateway", nil, nil, func(raw json.RawMessage, payload []byte) {
		var event gatewayEvent
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		switch event.Type {
		case "ready":
			select {
			case ready <- event.URL:
			default:
			}
		case "http":
			mu.Lock()
			if event.ID == 0 || active[event.ID] != nil {
				mu.Unlock()
				return
			}
			// The worker frees a slot before this side does, so it can admit a
			// request that finds no room here. Fail it rather than leave Pi waiting.
			if len(active) >= gatewaySlots || len(payload) > mcpserver.MaxRequestBytes {
				mu.Unlock()
				go func() {
					select {
					case <-started:
						reply := gatewayReply{Stream: streamID, ID: event.ID, Failed: true}
						_, _, _ = c.call(lifetime, "gatewayReply", reply, nil, nil)
					case <-lifetime.Done():
					}
				}()
				return
			}
			request, stop := context.WithCancel(lifetime)
			active[event.ID] = stop
			mu.Unlock()
			go func() {
				defer stop()
				defer func() {
					mu.Lock()
					delete(active, event.ID)
					mu.Unlock()
				}()
				select {
				case <-started:
					c.relayGateway(request, target, streamID, event, payload)
				case <-request.Done():
				}
			}()
		case "cancel":
			mu.Lock()
			if stop := active[event.ID]; stop != nil {
				stop()
			}
			mu.Unlock()
		}
	})
	if err != nil {
		cancel()
		return nil, err
	}
	streamID = id
	close(started)
	gateway := &Gateway{owner: c, cancel: cancel, done: make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		_, _, err := c.await(lifetime, id, call)
		cancel()
		finished <- err
		close(gateway.done)
	}()
	select {
	case gateway.URL = <-ready:
		return gateway, nil
	case err := <-finished:
		return nil, errors.Join(errors.New("worker: product gateway did not start"), err)
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

var gatewayHTTP = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{},
}

func (c *Client) relayGateway(ctx context.Context, target *url.URL, stream uint32, event gatewayEvent, body []byte) {
	send := func(reply gatewayReply, data []byte) bool {
		reply.Stream, reply.ID = stream, event.ID
		_, _, err := c.call(ctx, "gatewayReply", reply, data, nil)
		return err == nil
	}
	switch event.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		send(gatewayReply{Failed: true}, nil)
		return
	}
	req, err := http.NewRequestWithContext(ctx, event.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		send(gatewayReply{Failed: true}, nil)
		return
	}
	req.Header = requestHeaders(event.Header)
	response, err := gatewayHTTP.Do(req)
	if err != nil {
		send(gatewayReply{Failed: true}, nil)
		return
	}
	defer response.Body.Close()
	headers := selectedHeaders(response.Header, "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Cache-Control", "Allow")
	if !send(gatewayReply{Status: response.StatusCode, Header: headers}, nil) {
		return
	}
	data := make([]byte, gatewayChunkBytes)
	for {
		n, err := response.Body.Read(data)
		if n > 0 && !send(gatewayReply{}, data[:n]) {
			return
		}
		if err != nil {
			send(gatewayReply{End: errors.Is(err, io.EOF), Failed: !errors.Is(err, io.EOF)}, nil)
			return
		}
	}
}

func requestHeaders(source http.Header) http.Header {
	return selectedHeaders(source, "Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id", "Origin")
}

func selectedHeaders(source http.Header, names ...string) http.Header {
	headers := make(http.Header)
	for _, name := range names {
		if values := source.Values(name); len(values) > 0 {
			headers[http.CanonicalHeaderKey(name)] = values
		}
	}
	return headers
}

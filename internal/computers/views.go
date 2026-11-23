package computers

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/worker"
)

// view is one viewer session (View in views.ts): a user's window onto a Bot's
// browser or desktop, holding the control lease while it takes over.
type view struct {
	id          string
	botID       int64
	service     string
	computerID  string
	controller  bool // holds the control lease (takeover or manual)
	lease       int64
	taking      bool // a takeover request is being granted to this viewer
	requestID   string
	cursorColor string
	done        chan struct{}
	closeOnce   sync.Once
}

// frames is the worker's view stream reply shape.
type frames struct {
	ID       string            `json:"id,omitempty"`
	Messages []json.RawMessage `json:"messages,omitempty"`
	Closed   bool              `json:"closed,omitempty"`
}

// views tracks open viewers (ComputerViews in views.ts).
type views struct {
	s *services

	mu       sync.Mutex
	sessions map[string]*view
	seq      int64
}

func newViews(s *services) *views {
	return &views{s: s, sessions: map[string]*view{}}
}

// frame is one computer.watch stream event to the client.
type frame struct {
	Type    string          `json:"type"` // opened, message, closed, error
	ViewID  string          `json:"viewId,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// open starts a viewer session: resolves the binding, reserves the worker's
// view session and registers the viewer (views.ts open).
func (v *views) open(ctx context.Context, botID int64, service, cursorColor string) (*view, []byte, error) {
	record, _, err := v.s.binding(ctx, botID, service)
	if err != nil {
		return nil, nil, err
	}
	w, dir, err := v.s.workerFor(ctx, botID, service)
	if err != nil {
		return nil, nil, err
	}
	v.mu.Lock()
	v.seq++
	id := strconv.FormatInt(v.seq, 10)
	session := &view{
		id:          id,
		botID:       botID,
		service:     service,
		computerID:  record.ID,
		cursorColor: cursorColor,
		done:        make(chan struct{}),
	}
	v.sessions[id] = session
	v.mu.Unlock()
	result, err := w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "view", View: "open", Service: service, Dir: dir,
		ViewID: id, CursorColor: cursorColor,
	})
	if err != nil {
		v.remove(session)
		return nil, nil, err
	}
	return session, result.Result, nil
}

// next waits for the worker's next frame batch (views.ts next).
func (v *views) next(ctx context.Context, id string) (frames, error) {
	session := v.get(id)
	if session == nil {
		return frames{Closed: true}, nil
	}
	w, dir, err := v.s.workerFor(ctx, session.botID, session.service)
	if err != nil {
		return frames{}, err
	}
	result, err := w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "view", View: "next", Service: session.service, Dir: dir, ViewID: id,
	})
	if err != nil {
		return frames{}, err
	}
	var out frames
	if len(result.Result) > 0 {
		if err := json.Unmarshal(result.Result, &out); err != nil {
			return frames{}, err
		}
	}
	out.ID = id
	return out, nil
}

// action forwards viewer input (views.ts action: take/back/text/key/pointer).
func (v *views) action(ctx context.Context, id string, message json.RawMessage) error {
	session := v.get(id)
	if session == nil {
		return rpc.NewError("NOT_FOUND", "viewer fechado.")
	}
	var envelope struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return err
	}
	switch envelope.Action {
	case "take":
		// Takeover: reserve this viewer's lease before the grant waits.
		lease, err := v.s.takeover(ctx, session.service, session.botID)
		if err != nil {
			return err
		}
		session.lease = lease
		session.controller = true
	case "back":
		if session.lease != 0 {
			v.s.release(session.service, session.lease)
			session.lease = 0
			session.controller = false
		}
	}
	w, dir, err := v.s.workerFor(ctx, session.botID, session.service)
	if err != nil {
		return err
	}
	// Input is only allowed while this viewer holds control; bot-owned input
	// from a viewer is dropped (viewAccess in services.ts).
	if !session.controller && envelope.Action != "ack" {
		ctl := v.s.controlOf(session.service)
		if !ctl.owner && ctl.lease != session.lease {
			return nil
		}
	}
	_, err = w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "view", View: "send", Service: session.service, Dir: dir,
		ViewID: id, Message: message,
	})
	return err
}

// close ends the viewer: releases its lease reservation and closes the
// worker's session. Closing a viewer does not complete a takeover request.
func (v *views) close(ctx context.Context, id string) {
	session := v.get(id)
	if session == nil {
		return
	}
	session.closeOnce.Do(func() {
		if session.lease != 0 {
			v.s.release(session.service, session.lease)
			session.lease = 0
		}
		v.remove(session)
		close(session.done)
		if w, dir, err := v.s.workerFor(ctx, session.botID, session.service); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = w.ComputerRequest(ctx, worker.ComputerInput{
				Action: "view", View: "close", Service: session.service, Dir: dir, ViewID: id,
			})
		}
	})
}

func (v *views) get(id string) *view {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sessions[id]
}

func (v *views) remove(session *view) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sessions[session.id] == session {
		delete(v.sessions, session.id)
	}
}

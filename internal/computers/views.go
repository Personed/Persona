package computers

import (
	"context"
	"encoding/json"
	"fmt"
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
	grant       chan struct{} // closed when a settling takeover ends (views.ts taking)
	grantErr    error         // the settled takeover's failure, if any
	taking      bool          // a takeover request is being granted to this viewer
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
}

func newViews(s *services) *views {
	return &views{s: s, sessions: map[string]*view{}}
}

// frame is one computer.watch stream event to the client: `ready` once with
// the viewer id, then one `frame` event per screen image (computer.ts watch).
type frame struct {
	Type  string          `json:"type"` // ready, frame
	ID    string          `json:"id,omitempty"`
	Frame json.RawMessage `json:"frame,omitempty"`
}

// newViewID is a UUIDv4, the shape the computer.input schema requires.
func newViewID() string {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// open starts a viewer session: resolves the binding, prepares the service so
// a browser that cannot produce frames fails here, reserves the worker's view
// session and registers the viewer (views.ts open).
func (v *views) open(ctx context.Context, botID int64, service, cursorColor string) (*view, error) {
	record, _, err := v.s.binding(ctx, botID, service)
	if err != nil {
		return nil, err
	}
	w, dir, err := v.s.workerFor(ctx, botID, service)
	if err != nil {
		return nil, err
	}
	prepared, err := w.ComputerRequest(ctx, worker.ComputerInput{Action: "prepare", Service: service, Dir: dir})
	var state struct {
		Available     bool    `json:"available"`
		Error         *string `json:"error"`
		SharedDesktop *bool   `json:"sharedDesktop"`
	}
	if err == nil && len(prepared.Result) > 0 {
		err = json.Unmarshal(prepared.Result, &state)
	}
	if err != nil {
		return nil, err
	}
	if !state.Available {
		message := "电脑服务不可用。"
		if state.Error != nil {
			message = *state.Error
		}
		return nil, errUnavailable(message)
	}
	// A browser that does not share the desktop holds its own control lane
	// (services.ts isolatedBrowsers on the prepare result).
	if service == "browser" {
		v.s.setIsolated(botID, state.SharedDesktop != nil && !*state.SharedDesktop)
	}
	id := newViewID()
	session := &view{
		id:          id,
		botID:       botID,
		service:     service,
		computerID:  record.ID,
		cursorColor: cursorColor,
		done:        make(chan struct{}),
	}
	v.mu.Lock()
	v.sessions[id] = session
	v.mu.Unlock()
	if _, err := w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "view", View: "open", Service: service, Dir: dir,
		ViewID: id, CursorColor: cursorColor,
	}); err != nil {
		v.remove(session)
		return nil, err
	}
	return session, nil
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
		Type string `json:"type"` // ack, take, back, text, key, pointer (viewer-types.ts)
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return err
	}
	switch envelope.Type {
	case "take":
		// A second take waits for the grant in flight (views.ts awaits
		// view.taking); a held lease is reused.
		if grant := session.grant; grant != nil {
			<-grant
			if session.grantErr != nil {
				return session.grantErr
			}
		}
		if session.lease != 0 {
			return nil
		}
		session.grant = make(chan struct{})
		lease := v.s.nextLease()
		session.lease = lease
		err := v.s.takeover(ctx, session.service, session.botID, lease)
		session.grantErr = err
		close(session.grant)
		session.grant = nil
		if err != nil {
			session.lease = 0
			return err
		}
		// A viewer that closed during the grant released nothing it never
		// saw; give the claimed lane back (views.ts sessions.has).
		if v.get(id) == nil {
			v.s.release(session.service, session.botID, lease)
			return rpc.NewError("GONE", "画面连接已断开。")
		}
		session.controller = true
		return nil
	case "back":
		// views.ts awaits a grant in flight before handing back.
		if grant := session.grant; grant != nil {
			<-grant
			if session.grantErr != nil {
				return session.grantErr
			}
		}
		if session.lease == 0 {
			return nil
		}
		w, dir, err := v.s.workerFor(ctx, session.botID, session.service)
		if err != nil {
			return err
		}
		// The worker clears a held pointer while the lease still stands;
		// views.ts sends "back" before releasing.
		if _, err := w.ComputerRequest(ctx, worker.ComputerInput{
			Action: "view", View: "send", Service: session.service, Dir: dir,
			ViewID: id, Message: message,
		}); err != nil {
			return err
		}
		v.s.release(session.service, session.botID, session.lease)
		session.lease = 0
		session.controller = false
		return nil
	}
	w, dir, err := v.s.workerFor(ctx, session.botID, session.service)
	if err != nil {
		return err
	}
	// Input requires this viewer's granted lease, as viewAccess in
	// services.ts; a viewer never drives a Bot-owned computer.
	ctl := v.s.controlOf(session.service, session.botID)
	if ctl.lease != 0 && ctl.lease != session.lease {
		return rpc.NewError("CONFLICT", "用户正在另一窗口接管。请等待交还控制权。")
	}
	if session.lease != 0 && ctl.lease != session.lease {
		return rpc.NewError("CONFLICT", "这次接管已失效。")
	}
	if envelope.Type != "ack" && session.grant != nil {
		return rpc.NewError("CONFLICT", "正在交接控制权。请等待。")
	}
	if envelope.Type != "ack" && session.lease == 0 {
		return rpc.NewError("CONFLICT", "请先接管，再操作电脑。")
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
		// views.ts close awaits a grant in flight before releasing.
		if grant := session.grant; grant != nil {
			<-grant
		}
		if session.lease != 0 {
			v.s.release(session.service, session.botID, session.lease)
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

package computers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/takeover"
	"github.com/Personed/Persona/internal/worker"
)

// newRequestID mints a uuid-shaped id without a dependency.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// Handoff implements takeover.Handoff for the computer runtime: it posts the
// stored chat card and holds the call until the user returns control
// (runtime.ts invoke of computer_request_takeover).
func (c *Catalog) Handoff(approver *approval.Service) takeover.Handoff {
	return func(ctx context.Context, request takeover.Request) (bool, error) {
		binding, err := c.Binding(ctx, request.BotID)
		if err != nil {
			return false, err
		}
		if binding.ComputerID == nil || !binding.Services[request.Service] {
			return false, takeover.ErrUnavailable
		}
		// The browser card carries the current page address.
		var url string
		if request.Service == "browser" {
			if w, dir, err := c.svcs.workerFor(ctx, request.BotID, "browser"); err == nil {
				result, err := w.ComputerRequest(ctx, worker.ComputerInput{
					Action: "call", Service: "browser", Dir: dir,
					TaskID: request.TaskID, CallID: request.CallID + ":page",
					Name: "agent_browser_get_url",
				})
				if err == nil {
					var page struct {
						StructuredContent struct {
							Response struct {
								Data struct {
									URL string `json:"url"`
								} `json:"data"`
							} `json:"response"`
						} `json:"structuredContent"`
					}
					if json.Unmarshal(result.Result, &page) == nil && strings.HasPrefix(page.StructuredContent.Response.Data.URL, "http") {
						url = page.StructuredContent.Response.Data.URL
					}
				}
			}
		}
		id := newRequestID()
		title := "Computer"
		if request.Service == "browser" {
			title = "Browser"
		}
		// A preview frame shows the live screen on the card, like views.ts
		// capture; a computer that cannot produce one just skips it.
		preview, _ := c.vw.capture(ctx, request.BotID, request.Service)
		answer, err := approver.Request(ctx, request.BotID, approval.Request{
			ID: id, Title: title, Detail: request.Reason, Kind: approval.Permission,
			Preview: preview,
			Takeover: &approval.Takeover{
				ID: id, Service: request.Service, ComputerID: *binding.ComputerID,
				Revision: binding.Revision, State: "waiting", URL: url,
			},
		})
		if err != nil {
			return false, err
		}
		// The allowed answer already released the viewer lease: the
		// engines.answer procedure finishes the live request before it
		// resolves the pending one (runtime.ts execution.answer →
		// views.finishRequest → pending.resolve).
		return answer.Allow, nil
	}
}

// TakeRequest grants the request's takeover to a viewer (runtime.ts
// takeRequest → views.takeRequest): it reserves the viewer's lease before the
// grant waits and marks the request's card granted.
func (c *Catalog) TakeRequest(ctx context.Context, botID int64, requestID, viewID string, approver *approval.Service) error {
	request, err := approver.Pending(ctx, botID)
	if err != nil {
		return err
	}
	var card *approval.Request
	for i := range request {
		if request[i].ID == requestID && request[i].Takeover != nil {
			card = &request[i]
		}
	}
	if card == nil {
		return rpc.NewError("BAD_REQUEST", "接管请求已失效。")
	}
	// The binding must not have changed since the card was posted.
	binding, err := c.Binding(ctx, botID)
	if err != nil {
		return err
	}
	if binding.ComputerID == nil || *binding.ComputerID != card.Takeover.ComputerID ||
		binding.Revision != card.Takeover.Revision {
		return rpc.NewError("BAD_REQUEST", "工作电脑配置已改变。")
	}
	session := c.vw.get(viewID)
	if session == nil {
		return rpc.NewError("NOT_FOUND", "viewer fechado.")
	}
	if session.botID != botID || session.service != card.Takeover.Service {
		return rpc.NewError("FORBIDDEN", "viewer 不属于该请求。")
	}
	// A grant already settling reports its real outcome, like views.ts take
	// awaiting view.taking.
	session.mu.Lock()
	for session.grant != nil {
		grant := session.grant
		session.mu.Unlock()
		<-grant.done
		session.mu.Lock()
		if grant.err != nil {
			session.mu.Unlock()
			return grant.err
		}
	}
	if session.lease == 0 {
		grant := &pendingGrant{done: make(chan struct{})}
		session.grant = grant
		lease := c.svcs.nextLease()
		session.lease = lease
		session.mu.Unlock()
		err := c.svcs.takeover(ctx, session.service, botID, lease)
		session.mu.Lock()
		grant.err = err
		close(grant.done)
		session.grant = nil
		if err != nil {
			session.lease = 0
			session.mu.Unlock()
			return err
		}
		session.mu.Unlock()
		// A viewer that closed during the grant released nothing it never
		// saw; give the claimed lane back.
		if c.vw.get(viewID) == nil {
			c.svcs.release(session.service, botID, lease)
			return rpc.NewError("GONE", "画面连接已断开。")
		}
		session.mu.Lock()
		session.controller = true
	}
	session.mu.Unlock()
	// Concurrent requests from the owning viewer reuse the live grant.
	session.taking = true
	session.requestID = requestID
	// The card flips to "controlling" while the viewer holds control, as
	// catalog.ts commits it through the views callback.
	if err := approver.SetTakeover(ctx, botID, requestID, true); err != nil {
		return err
	}
	return nil
}

// FinishRequest completes the live request owned by the viewer (runtime.ts
// answer → views.finishRequest). Only an allowed answer settles it.
func (c *Catalog) FinishRequest(_ context.Context, botID int64, requestID string) error {
	c.vw.mu.Lock()
	var owner *view
	for _, session := range c.vw.sessions {
		if session.requestID == requestID && session.botID == botID {
			owner = session
		}
	}
	c.vw.mu.Unlock()
	if owner == nil {
		return rpc.NewError("BAD_REQUEST", "接管请求已失效。")
	}
	owner.taking = false
	owner.requestID = ""
	if owner.lease != 0 {
		c.svcs.release(owner.service, owner.botID, owner.lease)
		owner.lease = 0
		owner.controller = false
	}
	return nil
}

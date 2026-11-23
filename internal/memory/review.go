package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Personed/Persona/internal/engine"
	"github.com/Personed/Persona/internal/mcpserver"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

// Background review limits, shared with the TypeScript backend (harness/task.ts,
// harness/execution.ts and engineLimits.compactionMs).
const (
	// ReviewTurns is how many completed turns make one review due.
	ReviewTurns = 10
	// ReviewCalls bounds the tool calls of one review.
	ReviewCalls = 12
	// ReviewTimeout bounds one review, from session start to settlement.
	ReviewTimeout = 120 * time.Second
	// ReviewHistoryBytes bounds the conversation packet one review reads.
	ReviewHistoryBytes = 6000
)

// SnapshotDocKind is the task-scoped document that keeps a task's profile snapshot
// and, for a review task, its native session and dispatch state.
const SnapshotDocKind = "persona.memory-snapshot"

// gatewayEnv is the session variable Pi reads the Persona MCP gateway URL from
// (harness/providers/pi.ts).
const gatewayEnv = "PERSONA_PI_MCP_URL"

const toolPrefix = "memory_"

const reviewInstructions = "Review the recent conversation. Save only durable facts. Put cross-task user facts in profile and task-specific procedures in work notes. Keep facts declarative. Do not change or delete user-authored memories. Use only memory tools.\n\n"

// Review failures.
var (
	// ErrReviewTimeout is the cause of a review stopped at ReviewTimeout.
	ErrReviewTimeout = errors.New("memory: background review timed out")
	// ErrReviewUnconfirmed refuses a review task whose prompt was already sent: its
	// outcome is unknown, so the prompt is not sent again.
	ErrReviewUnconfirmed = errors.New("回顾结果未确认，不重复发送。")
)

// ReviewState is the conversation-scoped "persona.memory-review" document. Through
// is the last entry a completed review covered.
type ReviewState struct {
	Enabled   bool           `json:"enabled"`
	Turns     int            `json:"turns"`
	Through   *store.EntryID `json:"through"`
	Reviewing bool           `json:"reviewing"`
}

type reviewSnapshot struct {
	Text       string `json:"text"`
	Dispatched bool   `json:"dispatched,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	TurnID     string `json:"turnId,omitempty"`
}

func (s *Service) loadReview(ctx context.Context, botID int64) (store.Document, ReviewState, error) {
	// A Bot's conversation carries the Bot's ID.
	scope := store.Scope{Kind: "conversation", ConversationID: store.ConversationID(botID)}
	review := ReviewState{Enabled: true}
	meta, err := s.read(ctx, ReviewDocKind, scope, &review)
	if meta.ID == 0 {
		meta = store.Document{Kind: ReviewDocKind, Scope: scope, History: "latest", Fork: "initial"}
	}
	return meta, review, err
}

// changeReview applies change to a Bot's review state. It holds mu, so a memory
// write never interleaves with the start or end of a review.
func (s *Service) changeReview(ctx context.Context, botID int64, change func(*ReviewState)) (ReviewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, review, err := s.loadReview(ctx, botID)
	if err != nil {
		return ReviewState{}, err
	}
	before := review
	change(&review)
	if meta.ID != 0 && review == before {
		return review, nil
	}
	return review, s.write(ctx, &meta, review)
}

// ReviewEnabled reports whether background review runs for a Bot. It defaults to true.
func (s *Service) ReviewEnabled(ctx context.Context, botID int64) (bool, error) {
	_, review, err := s.loadReview(ctx, botID)
	return review.Enabled, err
}

// SetReviewEnabled turns background review on or off. Turning it off drops the counted turns.
func (s *Service) SetReviewEnabled(ctx context.Context, botID int64, enabled bool) error {
	_, err := s.changeReview(ctx, botID, func(review *ReviewState) {
		review.Enabled = enabled
		if !enabled {
			review.Turns = 0
		}
	})
	return err
}

// TurnStarted records that an ordinary task took over the Bot, so a review that
// was interrupted without cleanup stops guarding the Bot's writes.
func (s *Service) TurnStarted(ctx context.Context, botID int64) error {
	_, err := s.changeReview(ctx, botID, func(review *ReviewState) { review.Reviewing = false })
	return err
}

// TurnCompleted counts one answered user turn. Failed turns, operations and routine
// runs are not counted. It reports whether a review is due: ReviewTurns turns are
// counted and idle says the Bot has no other task. The caller then schedules one
// background review task up to the turn's answer entry.
func (s *Service) TurnCompleted(ctx context.Context, botID int64, idle bool) (bool, error) {
	review, err := s.changeReview(ctx, botID, func(review *ReviewState) {
		if review.Enabled {
			review.Turns++
		}
	})
	return review.Enabled && review.Turns >= ReviewTurns && idle, err
}

// ReviewOptions describe one background review task.
type ReviewOptions struct {
	Engine engine.Engine
	// Registry holds the Bot's product tools. The review reaches only its memory tools.
	Registry tools.Registry
	BotID    int64
	TaskID   int64
	// Through is the answer entry of the turn that made the review due. A completed
	// review records it as covered.
	Through store.EntryID
	// History returns the conversation packet after an entry (0 for the start) within
	// budget bytes and without tool history. "" means there is nothing to hand off.
	History func(ctx context.Context, after store.EntryID, budget int) (string, error)
	// Session supplies WorkDir, Model and ThinkingLevel. Review sets the other fields.
	Session engine.SessionOptions

	timeout time.Duration
}

// Review runs one background review in its own native session: the engine reads the
// recent conversation and saves durable facts through the memory tools only, within
// ReviewCalls calls and ReviewTimeout. Records the user wrote stay unchanged.
//
// The caller runs it as the Bot's only task and cancels ctx when the user sends new
// input. Only a review that settles without error consumes ReviewTurns counted turns
// and advances the covered entry. A disabled review returns nil without running.
func (s *Service) Review(ctx context.Context, options ReviewOptions) (err error) {
	state, err := s.changeReview(ctx, options.BotID, func(review *ReviewState) { review.Reviewing = review.Enabled })
	if err != nil || !state.Enabled {
		return err
	}
	completed := false
	defer func() {
		_, finishErr := s.changeReview(context.WithoutCancel(ctx), options.BotID, func(review *ReviewState) {
			review.Reviewing = false
			if completed {
				if options.Through != 0 {
					review.Through = &options.Through
				}
				review.Turns = max(0, review.Turns-ReviewTurns)
			}
		})
		err = errors.Join(err, finishErr)
	}()
	ctx, cancel := context.WithTimeoutCause(ctx, cmp.Or(options.timeout, ReviewTimeout), ErrReviewTimeout)
	defer cancel()
	var after store.EntryID
	if state.Through != nil {
		after = *state.Through
	}
	if err := s.review(ctx, options, after); err != nil {
		return err
	}
	completed = true
	return nil
}

func (s *Service) review(ctx context.Context, options ReviewOptions, after store.EntryID) (err error) {
	scope := store.Scope{Kind: "task", TaskID: store.TaskID(options.TaskID)}
	var snapshot reviewSnapshot
	meta, err := s.read(ctx, SnapshotDocKind, scope, &snapshot)
	if err != nil {
		return err
	}
	if snapshot.Dispatched {
		return ErrReviewUnconfirmed
	}
	record := func() error { return s.write(ctx, &meta, snapshot) }
	if meta.ID == 0 {
		// The profile snapshot stays stable for the task, as for an ordinary task.
		_, doc, err := s.load(ctx)
		if err != nil {
			return err
		}
		meta = store.Document{Kind: SnapshotDocKind, Scope: scope}
		snapshot.Text = Snapshot(doc.Items, options.BotID)
		if err := record(); err != nil {
			return err
		}
	}

	registry := &reviewRegistry{Registry: options.Registry}
	definitions, err := registry.List(ctx, options.BotID)
	if err != nil {
		return err
	}
	// Nobody watches a background review, so its memory calls need no confirmation.
	// The Service still refuses changes to records the user wrote.
	allow := func(context.Context, tools.Call, tools.Definition) (bool, error) { return true, nil }
	gateway, err := mcpserver.Open(ctx, mcpserver.Options{Registry: registry, BotID: options.BotID, TaskID: options.TaskID, Approve: allow})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gateway.Close()) }()

	session := options.Session
	session.BotID = options.BotID
	session.SessionID = snapshot.SessionID
	session.SystemPrompt = reviewInstructions + snapshot.Text
	session.Tools = make([]engine.ToolSpec, len(definitions))
	for i, definition := range definitions {
		session.Tools[i] = engine.ToolSpec{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema}
	}
	session.Env = maps.Clone(session.Env)
	if session.Env == nil {
		session.Env = map[string]string{}
	}
	session.Env[gatewayEnv] = gateway.URL
	native, err := options.Engine.Start(ctx, session)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, native.Close()) }()
	if snapshot.SessionID != native.ID() {
		snapshot.SessionID = native.ID()
		if err := record(); err != nil {
			return err
		}
	}
	if err := gateway.Ready(ctx); err != nil {
		return err
	}
	history, err := options.History(ctx, after, ReviewHistoryBytes)
	if err != nil {
		return err
	}
	snapshot.Dispatched = true
	if err := record(); err != nil {
		return err
	}
	prompt := engine.Prompt{
		RequestID: fmt.Sprintf("memory-review:%d", options.TaskID),
		Text:      "Quoted conversation data for memory review:\n" + cmp.Or(history, "{}"),
	}
	if err = native.Prompt(ctx, prompt); err == nil {
		err = settle(ctx, native)
	}
	if err != nil && ctx.Err() != nil {
		// The turn may still run: stop it and its process tree.
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
		defer cancel()
		err = errors.Join(err, native.Abort(stop))
	}
	return err
}

const abortTimeout = 10 * time.Second

// settle waits for the turn to settle and reports how its last assistant message
// stopped, as the Pi provider does (harness/providers/pi.ts).
func settle(ctx context.Context, session engine.Session) error {
	var failure error
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case event, open := <-session.Events():
			if !open {
				return errors.New("memory: review session closed before it settled")
			}
			switch event.Type {
			case "message_end":
				var frame struct {
					Message struct {
						Role         string `json:"role"`
						StopReason   string `json:"stopReason"`
						ErrorMessage string `json:"errorMessage"`
					} `json:"message"`
				}
				if json.Unmarshal(event.Data, &frame) != nil || frame.Message.Role != "assistant" {
					continue
				}
				switch frame.Message.StopReason {
				case "stop":
					failure = nil
				case "error":
					failure = errors.New(cmp.Or(frame.Message.ErrorMessage, "回复失败，原因待确认"))
				case "aborted":
					failure = errors.New("工作已停止。")
				}
			case "agent_settled":
				return failure
			}
		}
	}
}

// reviewRegistry limits a registry to what a background review may do.
type reviewRegistry struct {
	tools.Registry
	calls atomic.Int64
}

func (r *reviewRegistry) List(ctx context.Context, botID int64) ([]tools.Definition, error) {
	definitions, err := r.Registry.List(ctx, botID)
	return slices.DeleteFunc(definitions, func(definition tools.Definition) bool {
		return !strings.HasPrefix(definition.Name, toolPrefix)
	}), err
}

func (r *reviewRegistry) Call(ctx context.Context, call tools.Call, approve tools.Approver) (tools.Result, error) {
	if !strings.HasPrefix(call.Name, toolPrefix) {
		return tools.Result{}, errors.New("后台回顾只能调用记忆工具。")
	}
	if r.calls.Add(1) > ReviewCalls {
		return tools.Result{}, errors.New("本次回顾已达到工具调用上限。")
	}
	return r.Registry.Call(ctx, call, approve)
}

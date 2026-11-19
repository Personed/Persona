// Package approval is the shared approval flow for product tools, routine changes,
// computer actions and engine requests.
//
// Pending requests live in the conversation-scoped "persona.computer-requests"
// document and settled ones become "persona.engine-message" entries, matching the
// TypeScript backend (packages/agent/src/computer/runtime.ts), so the transcript
// and imported databases keep their shape.
package approval

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

// Record kinds, matching pi-durable.
const (
	DocKind      = "persona.computer-requests"
	MessageKind  = "persona.engine-message"
	computersDoc = "persona.computers"
)

// Request kinds.
const (
	Permission = "permission"
	Question   = "question"
)

// Timeout is how long a request waits for an answer before it expires.
const Timeout = 10 * time.Minute

var (
	// ErrExpired means the request is not pending for this Bot or is already being settled.
	ErrExpired = errors.New("approval: request is no longer pending")
	// ErrDuplicate means a request with this ID is already pending.
	ErrDuplicate = errors.New("approval: request ID is already pending")
)

// Field is one labelled value shown on a request.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Option is one choice of a request or question.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionItem is one question of a Question request. Value is a string or a string array.
type QuestionItem struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Options    []Option        `json:"options,omitempty"`
	Multiple   bool            `json:"multiple,omitempty"`
	AllowOther bool            `json:"allowOther,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	Multiline  bool            `json:"multiline,omitempty"`
	Required   bool            `json:"required,omitempty"`
}

// Request asks the user to allow an action or answer questions.
type Request struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Detail       string         `json:"detail"`
	Kind         string         `json:"kind"`
	Fields       []Field        `json:"fields,omitempty"`
	ConfirmLabel string         `json:"confirmLabel,omitempty"`
	URL          string         `json:"url,omitempty"`
	Questions    []QuestionItem `json:"questions,omitempty"`
	Options      []Option       `json:"options,omitempty"`
	// Automatic lets the Bot's "auto" approval mode allow the request without asking.
	Automatic bool `json:"automatic,omitempty"`
	// RequestedAt is set by Request, in Unix milliseconds.
	RequestedAt int64 `json:"requestedAt,omitempty"`
}

// Answer is the user's reply.
type Answer struct {
	Allow    bool                `json:"allow"`
	Text     string              `json:"text,omitempty"`
	Answers  map[string][]string `json:"answers,omitempty"`
	OptionID string              `json:"optionId,omitempty"`
}

// Service holds the pending requests of every Bot.
type Service struct {
	store   store.Store
	now     func() time.Time
	timeout time.Duration
	// mu guards pending and serializes read-modify-write cycles; store.Store has no conditional writes.
	mu      sync.Mutex
	pending map[string]*waiter
}

type waiter struct {
	botID int64
	// reply receives the answer, or nil when the request is withdrawn.
	reply      chan *Answer
	completing bool
}

// NewService returns a Service. now defaults to time.Now and timeout to Timeout.
func NewService(records store.Store, now func() time.Time, timeout time.Duration) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: records, now: now, timeout: cmp.Or(timeout, Timeout), pending: map[string]*waiter{}}
}

// Request stores the request and blocks until the user answers, the request expires or ctx ends.
// An expired request returns a denying Answer.
func (s *Service) Request(ctx context.Context, botID int64, request Request) (Answer, error) {
	request.Kind = cmp.Or(request.Kind, Permission)
	if request.ID == "" || request.Title == "" || (request.Kind != Permission && request.Kind != Question) {
		return Answer{}, errors.New("approval: request needs an ID, a title and a known kind")
	}
	if request.Automatic {
		auto, err := s.automatic(ctx, botID)
		if err != nil || auto {
			return Answer{Allow: auto}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Answer{}, err
	}
	request.RequestedAt = s.now().UnixMilli()
	value, err := json.Marshal(request)
	if err != nil {
		return Answer{}, err
	}
	pending, err := s.open(ctx, botID, request.ID, value)
	if err != nil {
		return Answer{}, err
	}
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	var answer *Answer
	select {
	case answer = <-pending.reply:
	case <-ctx.Done():
	case <-timer.C:
	}
	// The request must leave the store even though ctx may have ended.
	answer, err = s.settle(context.WithoutCancel(ctx), botID, request, value, pending, answer)
	if err != nil {
		return Answer{}, err
	}
	if answer == nil {
		return Answer{}, ctx.Err()
	}
	return *answer, nil
}

// Answer resolves a pending request of this Bot.
func (s *Service) Answer(botID int64, id string, answer Answer) error {
	return s.reply(botID, id, &answer)
}

// Expire withdraws a pending request of this Bot, for example when the engine closes it.
func (s *Service) Expire(botID int64, id string) {
	_ = s.reply(botID, id, nil)
}

func (s *Service) reply(botID int64, id string, answer *Answer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending[id]
	if pending == nil || pending.botID != botID || pending.completing {
		return ErrExpired
	}
	pending.completing = true
	pending.reply <- answer
	return nil
}

// Pending returns the Bot's stored requests, oldest first.
func (s *Service) Pending(ctx context.Context, botID int64) ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, doc, err := s.load(ctx, botID)
	if err != nil {
		return nil, err
	}
	requests := make([]Request, len(doc.approvals))
	for i, value := range doc.approvals {
		if err := json.Unmarshal(value, &requests[i]); err != nil {
			return nil, fmt.Errorf("approval: decode %s: %w", DocKind, err)
		}
	}
	return requests, nil
}

// Approver asks the user before each mutating product tool call. These requests
// are never automatic: the registry cannot tell which tools "auto" mode may allow.
func (s *Service) Approver() tools.Approver {
	return func(ctx context.Context, call tools.Call, definition tools.Definition) (bool, error) {
		answer, err := s.Request(ctx, call.BotID, Request{
			ID:     "tool:" + call.CallID,
			Title:  definition.Name,
			Fields: fields(call.Input),
			Kind:   Permission,
		})
		return answer.Allow, err
	}
}

func fields(input json.RawMessage) []Field {
	var arguments map[string]json.RawMessage
	if json.Unmarshal(input, &arguments) != nil {
		return nil
	}
	result := make([]Field, 0, len(arguments))
	for label, value := range arguments {
		var text string
		if json.Unmarshal(value, &text) != nil {
			text = string(value)
		}
		result = append(result, Field{Label: label, Value: text})
	}
	slices.SortFunc(result, func(a, b Field) int { return cmp.Compare(a.Label, b.Label) })
	return result
}

func (s *Service) open(ctx context.Context, botID int64, id string, value json.RawMessage) (*waiter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != nil {
		return nil, fmt.Errorf("%w: %q", ErrDuplicate, id)
	}
	meta, doc, err := s.load(ctx, botID)
	if err != nil {
		return nil, err
	}
	// A request left by an earlier process has no waiter; this one replaces it.
	doc.remove(id)
	doc.approvals = append(doc.approvals, value)
	if err := s.save(ctx, botID, meta, doc); err != nil {
		return nil, err
	}
	pending := &waiter{botID: botID, reply: make(chan *Answer, 1)}
	s.pending[id] = pending
	return pending, nil
}

// settle removes the request and records its outcome in the transcript in one commit.
func (s *Service) settle(ctx context.Context, botID int64, request Request, value json.RawMessage, pending *waiter, answer *Answer) (*Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, request.ID)
	pending.completing = true
	if answer == nil {
		// An answer accepted while the wait was ending still counts.
		select {
		case answer = <-pending.reply:
		default:
		}
	}
	meta, doc, err := s.load(ctx, botID)
	if err != nil {
		return nil, err
	}
	if !doc.remove(request.ID) {
		return answer, nil
	}
	state := "expired"
	if answer != nil {
		state = "denied"
		if answer.Allow {
			state = "answered"
		}
	}
	data, err := json.Marshal(message{
		ID:        "approval:" + request.ID + ":" + strconv.FormatInt(request.RequestedAt, 10),
		Role:      "assistant",
		Content:   []struct{}{},
		Timestamp: request.RequestedAt,
		Stopped:   state == "expired",
		Approval:  messageApproval{Request: value, State: state, Answer: answer},
	})
	if err != nil {
		return nil, err
	}
	id, err := s.store.MintID(ctx)
	if err != nil {
		return nil, err
	}
	entry := store.Write{Type: store.WriteEntry, Entry: &store.Entry{
		ID:             store.EntryID(id),
		ConversationID: store.ConversationID(botID),
		Kind:           MessageKind,
		Data:           data,
	}}
	return answer, s.save(ctx, botID, meta, doc, entry)
}

type message struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   []struct{}      `json:"content"`
	Timestamp int64           `json:"timestamp"`
	Pending   bool            `json:"pending"`
	Stopped   bool            `json:"stopped"`
	Approval  messageApproval `json:"approval"`
}

type messageApproval struct {
	Request json.RawMessage `json:"request"`
	State   string          `json:"state"`
	Answer  *Answer         `json:"answer,omitempty"`
}

// document keeps every field of the stored value so fields owned by other code survive a rewrite.
type document struct {
	fields    map[string]json.RawMessage
	approvals []json.RawMessage
}

func (d *document) remove(id string) bool {
	before := len(d.approvals)
	d.approvals = slices.DeleteFunc(d.approvals, func(value json.RawMessage) bool {
		var request struct {
			ID string `json:"id"`
		}
		return json.Unmarshal(value, &request) == nil && request.ID == id
	})
	return len(d.approvals) != before
}

func (s *Service) read(ctx context.Context, kind string, scope store.Scope, target any) (store.Document, error) {
	meta, err := s.store.FindDocument(ctx, kind, scope, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return store.Document{}, nil
	}
	if err != nil {
		return store.Document{}, err
	}
	value, err := s.store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return store.Document{}, err
	}
	if err := json.Unmarshal(value.Value, target); err != nil {
		return store.Document{}, fmt.Errorf("approval: decode %s: %w", kind, err)
	}
	return meta, nil
}

func scope(botID int64) store.Scope {
	return store.Scope{Kind: "conversation", ConversationID: store.ConversationID(botID)}
}

func (s *Service) load(ctx context.Context, botID int64) (store.Document, document, error) {
	doc := document{fields: map[string]json.RawMessage{}}
	meta, err := s.read(ctx, DocKind, scope(botID), &doc.fields)
	if err != nil {
		return store.Document{}, document{}, err
	}
	if approvals := doc.fields["approvals"]; approvals != nil {
		if err := json.Unmarshal(approvals, &doc.approvals); err != nil {
			return store.Document{}, document{}, fmt.Errorf("approval: decode %s: %w", DocKind, err)
		}
	}
	return meta, doc, nil
}

func (s *Service) save(ctx context.Context, botID int64, meta store.Document, doc document, extra ...store.Write) error {
	approvals, err := json.Marshal(append([]json.RawMessage{}, doc.approvals...))
	if err != nil {
		return err
	}
	doc.fields["approvals"] = approvals
	value, err := json.Marshal(doc.fields)
	if err != nil {
		return err
	}
	write := store.Write{Type: store.WriteDocumentChange, Document: &meta, Value: value}
	if meta.ID == 0 {
		id, err := s.store.MintID(ctx)
		if err != nil {
			return err
		}
		meta = store.Document{ID: store.DocumentID(id), Kind: DocKind, Scope: scope(botID), History: "latest", Fork: "initial"}
		write = store.Write{Type: store.WriteDocumentCreate, Document: &meta, Value: value}
	}
	_, _, err = s.store.Commit(ctx, []store.Batch{append(store.Batch{write}, extra...)})
	return err
}

// automatic reports whether the Bot's approval mode allows automatic requests without asking.
func (s *Service) automatic(ctx context.Context, botID int64) (bool, error) {
	var computers struct {
		Bindings map[string]struct {
			ApprovalMode string `json:"approvalMode"`
		} `json:"bindings"`
	}
	if _, err := s.read(ctx, computersDoc, store.Scope{Kind: "session"}, &computers); err != nil {
		return false, err
	}
	return computers.Bindings[strconv.FormatInt(botID, 10)].ApprovalMode == "auto", nil
}

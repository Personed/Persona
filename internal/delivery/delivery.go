// Package delivery puts product-tool results in front of the user: inline chat notices
// and delivered files. Updates and final answers stay native assistant messages.
package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/computer"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

var sessionScope = store.Scope{Kind: "session"}

// Record kinds shared with pi-durable databases.
const (
	MessageKind      = "persona.engine-message"
	CallsDocKind     = "persona.engine-product-calls"
	engineDocKind    = "persona.engine"
	routinesDocKind  = "persona.routines"
	computersDocKind = "persona.computers"
)

// Error is a failure the model can act on. Tools return it as an error result.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// Workspace is a Bot's working directory on its bound work computer.
type Workspace struct {
	Target computer.Target
	Dir    string
}

// Options configure a Service. Root is the durable files directory ("<database>.files").
type Options struct {
	Store     store.Store
	Root      string
	Workspace func(ctx context.Context, botID int64) (Workspace, error)
	Now       func() time.Time
}

// Service delivers product-tool messages and files to a Bot's chat.
type Service struct {
	options Options
	mu      sync.Mutex
}

// NewService returns a Service. Now defaults to time.Now.
func NewService(options Options) *Service {
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{options: options}
}

// Notice is a lightweight inline chat event.
type Notice struct {
	Text string `json:"text"`
}

// Message is what one product-tool call shows in the Bot's chat.
type Message struct {
	Notice      *Notice
	Attachments []Attachment
}

type chatMessage struct {
	ID          string       `json:"id"`
	TurnID      *string      `json:"turnId"`
	Hidden      bool         `json:"hidden"`
	Role        string       `json:"role"`
	Content     []struct{}   `json:"content"`
	Stopped     bool         `json:"stopped"`
	Timestamp   int64        `json:"timestamp"`
	ToolCallID  string       `json:"toolCallId"`
	ToolName    string       `json:"toolName"`
	ToolStatus  string       `json:"toolStatus"`
	IsError     bool         `json:"isError"`
	Notice      *Notice      `json:"notice,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Deliver appends the call's tool-result message to the Bot's chat. A call delivers at
// most once: the entry and its record in the task's delivered calls commit together.
func (s *Service) Deliver(ctx context.Context, call tools.Call, message Message) error {
	if message.Notice == nil && len(message.Attachments) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.options.Store
	digest := sha256.Sum256([]byte(call.CallID))
	key := hex.EncodeToString(digest[:])
	var calls struct {
		Delivered []string `json:"delivered"`
	}
	scope := store.Scope{Kind: "task", TaskID: store.TaskID(call.TaskID)}
	meta, err := s.document(ctx, CallsDocKind, scope, &calls)
	if err != nil {
		return err
	}
	if slices.Contains(calls.Delivered, key) {
		return nil
	}
	calls.Delivered = append(calls.Delivered, key)
	value, err := json.Marshal(calls)
	if err != nil {
		return err
	}
	callsWrite := store.Write{Type: store.WriteDocumentChange, Document: &meta, Value: value}
	if meta.ID == 0 {
		id, err := records.MintID(ctx)
		if err != nil {
			return err
		}
		meta = store.Document{ID: store.DocumentID(id), Kind: CallsDocKind, Scope: scope}
		callsWrite.Type = store.WriteDocumentCreate
	}

	conversation := store.ConversationID(call.BotID)
	var engine struct {
		ActiveTurn *string `json:"activeTurn"`
	}
	if _, err := s.document(ctx, engineDocKind, store.Scope{Kind: "conversation", ConversationID: conversation}, &engine); err != nil {
		return err
	}
	hidden, err := s.conditional(ctx, call.TaskID)
	if err != nil {
		return err
	}
	id, err := records.MintID(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(chatMessage{
		ID:          "product:" + strconv.FormatInt(id, 10),
		TurnID:      engine.ActiveTurn,
		Hidden:      hidden,
		Role:        "toolResult",
		Content:     []struct{}{},
		Timestamp:   s.options.Now().UnixMilli(),
		ToolCallID:  call.CallID,
		ToolName:    call.Name,
		ToolStatus:  "done",
		Notice:      message.Notice,
		Attachments: message.Attachments,
	})
	if err != nil {
		return err
	}
	entry := store.Entry{ID: store.EntryID(id), ConversationID: conversation, Kind: MessageKind, Data: data}
	_, _, err = records.Commit(ctx, []store.Batch{{{Type: store.WriteEntry, Entry: &entry}, callsWrite}})
	return err
}

// document decodes the current value of a document. A missing document leaves target
// unchanged and returns zero metadata.
func (s *Service) document(ctx context.Context, kind string, scope store.Scope, target any) (store.Document, error) {
	meta, err := s.options.Store.FindDocument(ctx, kind, scope, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return store.Document{}, nil
	}
	if err != nil {
		return store.Document{}, err
	}
	value, err := s.options.Store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return store.Document{}, err
	}
	if err := json.Unmarshal(value.Value, target); err != nil {
		return store.Document{}, fmt.Errorf("delivery: decode %s: %w", kind, err)
	}
	return meta, nil
}

// conditional reports whether the task runs a routine that reports only on a condition.
// Its messages stay out of chat until the routine decides to report.
func (s *Service) conditional(ctx context.Context, taskID int64) (bool, error) {
	task, err := s.options.Store.Task(ctx, store.TaskID(taskID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var input struct {
		RoutineRunID string `json:"routineRunId"`
	}
	if json.Unmarshal(task.Input, &input) != nil || input.RoutineRunID == "" {
		return false, nil
	}
	var routines struct {
		Runs []struct {
			ID       string `json:"id"`
			Delivery string `json:"delivery"`
		} `json:"runs"`
	}
	if _, err := s.document(ctx, routinesDocKind, sessionScope, &routines); err != nil {
		return false, err
	}
	for _, run := range routines.Runs {
		if run.ID == input.RoutineRunID {
			return run.Delivery == "conditional", nil
		}
	}
	return false, nil
}

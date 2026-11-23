// Package agent owns Bots, their chat projection, and their files, as
// packages/agent does for the TypeScript service. Its documents and file
// layout match the TypeScript runtime so either service reads the other's data.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Personed/Persona/internal/store"
)

// WorkError is a domain error with an oRPC code, as WorkError in
// packages/agent/src/work/state.ts: NOT_FOUND, CONFLICT or BAD_REQUEST.
type WorkError struct {
	Code    string
	Message string
}

func (e *WorkError) Error() string { return e.Message }

// ErrNoBot reports a Bot that is not in the directory.
var ErrNoBot = &WorkError{Code: "NOT_FOUND", Message: "Bot不存在。"}

// Doc is a typed document address, the defineDoc of pi-durable: a kind in a
// scope ("session", "conversation" or "task") with the value Initial returns
// while the document does not exist.
type Doc[T any] struct {
	Kind    string
	Scope   string
	History string
	Fork    string
	Initial func() T
}

func (d Doc[T]) scope(owner int64) store.Scope {
	switch d.Scope {
	case "conversation":
		return store.Scope{Kind: "conversation", ConversationID: store.ConversationID(owner)}
	case "task":
		return store.Scope{Kind: "task", TaskID: store.TaskID(owner)}
	}
	return store.Scope{Kind: "session"}
}

func (d Doc[T]) initial() T {
	if d.Initial != nil {
		return d.Initial()
	}
	var zero T
	return zero
}

// Harness reads and commits documents over a store. Commits are serialized:
// store.Store has no conditional writes, so every read-modify-write runs under
// one lock.
type Harness struct {
	store store.Store
	mu    sync.Mutex
}

// NewHarness returns a Harness over records.
func NewHarness(records store.Store) *Harness { return &Harness{store: records} }

// Store returns the underlying record store.
func (h *Harness) Store() store.Store { return h.store }

// Snapshot reads the current value of a document. A missing document reads as
// its initial value with ok false.
func Snapshot[T any](ctx context.Context, h *Harness, d Doc[T], owner int64) (value T, ok bool, err error) {
	value = d.initial()
	meta, err := h.store.FindDocument(ctx, d.Kind, d.scope(owner), "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	current, err := h.store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return value, false, err
	}
	if err := json.Unmarshal(current.Value, &value); err != nil {
		return value, false, fmt.Errorf("agent: decode %s: %w", d.Kind, err)
	}
	return value, true, nil
}

// Watch delivers a value after every commit until ctx ends. Register it before
// the first read so no commit is missed.
func (h *Harness) Watch(ctx context.Context) (<-chan store.Seq, error) { return h.store.Watch(ctx) }

// Tx collects the writes of one Commit.
type Tx struct {
	ctx    context.Context
	h      *Harness
	writes store.Batch
	docs   []*txDoc
}

type txDoc struct {
	meta     store.Document
	exists   bool
	original []byte
	value    any
}

// Edit returns the document's value for changing in tx. The value is written
// when the transaction commits, if it changed.
func Edit[T any](tx *Tx, d Doc[T], owner int64) (*T, error) {
	scope := d.scope(owner)
	for _, doc := range tx.docs {
		if doc.meta.Kind == d.Kind && doc.meta.Scope == scope {
			return doc.value.(*T), nil
		}
	}
	value := d.initial()
	doc := &txDoc{meta: store.Document{Kind: d.Kind, Scope: scope, History: d.History, Fork: d.Fork}}
	meta, err := tx.h.store.FindDocument(tx.ctx, d.Kind, scope, "", store.Current)
	switch {
	case err == nil:
		current, err := tx.h.store.Document(tx.ctx, meta.ID, store.Current)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(current.Value, &value); err != nil {
			return nil, fmt.Errorf("agent: decode %s: %w", d.Kind, err)
		}
		doc.meta, doc.exists = meta, true
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if doc.original, err = json.Marshal(value); err != nil {
		return nil, err
	}
	doc.value = &value
	tx.docs = append(tx.docs, doc)
	return &value, nil
}

// MintID reserves a record ID.
func (tx *Tx) MintID() (int64, error) { return tx.h.store.MintID(tx.ctx) }

// CreateConversation adds an ownerless conversation.
func (tx *Tx) CreateConversation() (store.ConversationID, error) {
	id, err := tx.MintID()
	if err != nil {
		return 0, err
	}
	conversation := &store.Conversation{ID: store.ConversationID(id)}
	tx.writes = append(tx.writes, store.Write{Type: store.WriteConversation, Conversation: conversation})
	return conversation.ID, nil
}

// Commit runs fn and stores its writes in one batch. An error from fn stores nothing.
func (h *Harness) Commit(ctx context.Context, fn func(tx *Tx) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	tx := &Tx{ctx: ctx, h: h}
	if err := fn(tx); err != nil {
		return err
	}
	batch := tx.writes
	for _, doc := range tx.docs {
		value, err := json.Marshal(doc.value)
		if err != nil {
			return err
		}
		if bytes.Equal(value, doc.original) {
			continue
		}
		write := store.Write{Type: store.WriteDocumentChange, Value: value}
		if !doc.exists {
			id, err := tx.MintID()
			if err != nil {
				return err
			}
			doc.meta.ID = store.DocumentID(id)
			write.Type = store.WriteDocumentCreate
		}
		meta := doc.meta
		write.Document = &meta
		batch = append(batch, write)
	}
	if len(batch) == 0 {
		return nil
	}
	_, _, err := h.store.Commit(ctx, []store.Batch{batch})
	return err
}

// Package storetest provides an in-memory store.Store for tests until the SQLite store (G1) lands.
//
// Documents are fully supported with full-value changes. Other record kinds are not
// stored yet: their writes are refused and their reads report store.ErrNotFound.
package storetest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/Personed/Persona/internal/store"
)

// ErrUnsupported is returned for writes the fake does not store yet.
var ErrUnsupported = errors.New("storetest: unsupported write")

// Store is an in-memory store.Store. The zero value is not usable; call New.
type Store struct {
	mu       sync.Mutex
	nextID   int64
	seq      store.Seq
	docs     map[store.DocumentID]*document
	watchers map[chan store.Seq]struct{}
}

type document struct {
	meta    store.Document
	version int
	value   []byte
}

// New returns an empty Store.
func New() *Store {
	return &Store{docs: map[store.DocumentID]*document{}, watchers: map[chan store.Seq]struct{}{}}
}

var _ store.Store = (*Store)(nil)

// Commit applies each batch atomically.
func (s *Store) Commit(_ context.Context, batches []store.Batch) (int, []store.Seq, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seqs := make([]store.Seq, 0, len(batches))
	for index, batch := range batches {
		seq := s.seq + 1
		staged := map[store.DocumentID]*document{}
		for _, write := range batch {
			if err := s.stage(staged, write, seq); err != nil {
				return index, seqs, err
			}
		}
		for id, doc := range staged {
			s.docs[id] = doc
		}
		s.seq = seq
		seqs = append(seqs, seq)
		for watcher := range s.watchers {
			select {
			case watcher <- seq:
			default:
			}
		}
	}
	return len(batches), seqs, nil
}

func (s *Store) stage(staged map[store.DocumentID]*document, write store.Write, seq store.Seq) error {
	if write.Document == nil {
		return fmt.Errorf("%w: %s", ErrUnsupported, write.Type)
	}
	id := write.Document.ID
	current := staged[id]
	if current == nil {
		current = s.docs[id]
	}
	switch write.Type {
	case store.WriteDocumentCreate:
		if current != nil {
			return fmt.Errorf("storetest: document %d exists", id)
		}
		meta := *write.Document
		meta.CreatedAt = seq
		staged[id] = &document{meta: meta, version: 1, value: slices.Clone(write.Value)}
	case store.WriteDocumentChange:
		if current == nil || current.meta.RetiredAt != nil {
			return store.ErrNotFound
		}
		if write.Ops != nil || write.Value == nil {
			return fmt.Errorf("%w: document patch", ErrUnsupported)
		}
		staged[id] = &document{meta: current.meta, version: current.version + 1, value: slices.Clone(write.Value)}
	case store.WriteDocumentRetire:
		if current == nil {
			return store.ErrNotFound
		}
		meta := current.meta
		meta.RetiredAt = &seq
		staged[id] = &document{meta: meta, version: current.version, value: current.value}
	default:
		return fmt.Errorf("%w: %s", ErrUnsupported, write.Type)
	}
	return nil
}

// MintID returns the next ID.
func (s *Store) MintID(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID, nil
}

// Watch delivers commit Seqs until ctx ends. Slow receivers miss Seqs.
func (s *Store) Watch(ctx context.Context) (<-chan store.Seq, error) {
	channel := make(chan store.Seq, 16)
	s.mu.Lock()
	s.watchers[channel] = struct{}{}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		delete(s.watchers, channel)
		s.mu.Unlock()
		close(channel)
	}()
	return channel, nil
}

// FindDocument returns the live document with this kind, scope and key.
func (s *Store) FindDocument(_ context.Context, kind string, scope store.Scope, key string, _ store.Seq) (store.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, doc := range s.docs {
		if doc.meta.Kind == kind && doc.meta.Scope == scope && doc.meta.Key == key && doc.meta.RetiredAt == nil {
			return doc.meta, nil
		}
	}
	return store.Document{}, store.ErrNotFound
}

// Document returns a document's latest value. History is not kept.
func (s *Store) Document(_ context.Context, id store.DocumentID, _ store.Seq) (store.DocumentValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[id]
	if !ok {
		return store.DocumentValue{}, store.ErrNotFound
	}
	return store.DocumentValue{Document: doc.meta, Version: doc.version, Value: slices.Clone(doc.value)}, nil
}

// Documents lists the live documents in a scope, ordered by ID, in one page.
func (s *Store) Documents(_ context.Context, query store.DocumentQuery, _ store.Scan) (store.Page[store.Document], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var page store.Page[store.Document]
	for _, doc := range s.docs {
		if doc.meta.Scope == query.Scope && (query.Kind == "" || doc.meta.Kind == query.Kind) && doc.meta.RetiredAt == nil {
			page.Items = append(page.Items, doc.meta)
		}
	}
	slices.SortFunc(page.Items, func(a, b store.Document) int { return cmp.Compare(a.ID, b.ID) })
	return page, nil
}

// Conversation is not stored yet.
func (s *Store) Conversation(context.Context, store.ConversationID) (store.Conversation, error) {
	return store.Conversation{}, store.ErrNotFound
}

// Conversations is not stored yet.
func (s *Store) Conversations(context.Context, store.ConversationQuery, store.Scan) (store.Page[store.Conversation], error) {
	return store.Page[store.Conversation]{}, nil
}

// Entry is not stored yet.
func (s *Store) Entry(context.Context, store.EntryID) (store.Entry, store.Seq, error) {
	return store.Entry{}, 0, store.ErrNotFound
}

// LatestHead is not stored yet.
func (s *Store) LatestHead(context.Context, store.ConversationID, store.EntryID) (store.Entry, error) {
	return store.Entry{}, store.ErrNotFound
}

// Entries is not stored yet.
func (s *Store) Entries(context.Context, store.EntryQuery, store.Scan) (store.Page[store.Entry], error) {
	return store.Page[store.Entry]{}, nil
}

// Task is not stored yet.
func (s *Store) Task(context.Context, store.TaskID) (store.Task, error) {
	return store.Task{}, store.ErrNotFound
}

// Tasks is not stored yet.
func (s *Store) Tasks(context.Context, store.TaskQuery, store.Scan) (store.Page[store.Task], error) {
	return store.Page[store.Task]{}, nil
}

// Submission is not stored yet.
func (s *Store) Submission(context.Context, store.SubmissionID) (store.Submission, error) {
	return store.Submission{}, store.ErrNotFound
}

// SubmissionByRequest is not stored yet.
func (s *Store) SubmissionByRequest(context.Context, store.ConversationID, string) (store.Submission, error) {
	return store.Submission{}, store.ErrNotFound
}

// Submissions is not stored yet.
func (s *Store) Submissions(context.Context, store.SubmissionQuery, store.Scan) (store.Page[store.Submission], error) {
	return store.Page[store.Submission]{}, nil
}

// Close is a no-op.
func (s *Store) Close() error { return nil }

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrDuplicateRequest means a submission already owns the request ID in this conversation.
// The original submission can be read with SubmissionByRequest.
var ErrDuplicateRequest = errors.New("store: duplicate request ID")

// Commit persists each batch atomically and stops at the first failure.
func (s *SQLite) Commit(ctx context.Context, batches []Batch) (int, []Seq, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	if s.closed {
		return 0, nil, ErrClosed
	}
	seqs := make([]Seq, 0, len(batches))
	for i, batch := range batches {
		seq, err := s.commitBatch(ctx, batch)
		if err != nil {
			return i, seqs, fmt.Errorf("store: batch %d: %w", i, err)
		}
		seqs = append(seqs, seq)
		s.publish(seq)
	}
	return len(seqs), seqs, nil
}

func (s *SQLite) commitBatch(ctx context.Context, batch Batch) (Seq, error) {
	var seq Seq
	err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		seq, err = applyBatch(ctx, tx, batch)
		return err
	})
	return seq, err
}

func applyBatch(ctx context.Context, tx *sql.Tx, batch Batch) (Seq, error) {
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return 0, err
	}
	var lastID int64
	var docs []Write
	for i, w := range batch {
		id, err := writeID(w)
		if err != nil {
			return 0, fmt.Errorf("write %d: %w", i, err)
		}
		lastID = max(lastID, id)
		if w.Document != nil {
			docs = append(docs, w)
			continue
		}
		if err := claimID(ctx, tx, id, string(w.Type)); err != nil {
			return 0, err
		}
		if err := writeRecord(ctx, tx, seq, w); err != nil {
			return 0, fmt.Errorf("write %d (%s): %w", i, w.Type, err)
		}
	}
	if err := writeDocuments(ctx, tx, seq, docs); err != nil {
		return 0, err
	}
	if lastID != 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE store_state SET last_id=max(last_id, ?) WHERE singleton=1", lastID); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

func writeID(w Write) (int64, error) {
	records := 0
	for _, set := range []bool{w.Conversation != nil, w.Entry != nil, w.Task != nil, w.Submission != nil, w.Document != nil} {
		if set {
			records++
		}
	}
	if records != 1 || (w.CopyFrom != nil) != (w.Type == WriteDocumentCopy) {
		return 0, errors.New("write must set exactly its record and only a copy can set CopyFrom")
	}
	if w.Type != WriteDocumentCreate && w.Type != WriteDocumentChange && (len(w.Value) != 0 || len(w.Ops) != 0) {
		return 0, errors.New("content is only allowed on document create or change")
	}
	if w.Type == WriteDocumentCreate && len(w.Ops) != 0 {
		return 0, errors.New("document creation requires a base value")
	}
	var id int64
	switch {
	case w.Type == WriteConversation && w.Conversation != nil:
		id = int64(w.Conversation.ID)
	case w.Type == WriteEntry && w.Entry != nil:
		id = int64(w.Entry.ID)
	case w.Type == WriteTask && w.Task != nil:
		id = int64(w.Task.ID)
	case w.Type == WriteSubmission && w.Submission != nil:
		id = int64(w.Submission.ID)
	case w.Document != nil:
		switch w.Type {
		case WriteDocumentCreate, WriteDocumentCopy, WriteDocumentChange, WriteDocumentRetire:
			id = int64(w.Document.ID)
		}
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid record for %q: positive ID required", w.Type)
	}
	return id, nil
}

func claimID(ctx context.Context, tx *sql.Tx, id int64, kind string) error {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO record_ids(id, kind) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET kind=excluded.kind WHERE record_ids.kind=excluded.kind`, id, kind)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("store: ID %d belongs to another record kind", id)
	}
	return err
}

func writeRecord(ctx context.Context, tx *sql.Tx, seq Seq, w Write) error {
	var record any
	var query string
	var args []any
	switch w.Type {
	case WriteConversation:
		c := w.Conversation
		var parent ForkPoint
		var owner TaskOwner
		if c.Parent != nil {
			parent = *c.Parent
		}
		if c.Owner != nil {
			owner = *c.Owner
		}
		record, args = c, []any{c.ID, parent.ConversationID, parent.At, owner.ConversationID, owner.TaskID}
		query = `INSERT INTO conversations VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET parent_id=excluded.parent_id, fork_at=excluded.fork_at,
			owner_conversation_id=excluded.owner_conversation_id, owner_task_id=excluded.owner_task_id, record=excluded.record`
	case WriteEntry:
		e := w.Entry
		record, args = e, []any{e.ID, e.ConversationID, seq, e.Head}
		query = "INSERT INTO entries VALUES (?, ?, ?, ?, ?)"
	case WriteTask:
		t := w.Task
		record, args = t, []any{t.ID, t.ConversationID, t.Kind, t.State.Status, t.AbortRequested, t.Background}
		query = `INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET conversation_id=excluded.conversation_id, kind=excluded.kind,
			status=excluded.status, abort_requested=excluded.abort_requested, background=excluded.background,
			record=excluded.record`
	case WriteSubmission:
		sub := w.Submission
		record, args = sub, []any{sub.ID, sub.ConversationID, sub.RequestID, sub.Status}
		query = `INSERT INTO submissions VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET status=excluded.status, record=excluded.record
			WHERE submissions.conversation_id=excluded.conversation_id AND submissions.request_id=excluded.request_id
			ON CONFLICT(conversation_id, request_id) WHERE request_id <> '' DO NOTHING`
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, query, append(args, string(body))...)
	if err != nil {
		return err
	}
	if w.Type == WriteSubmission {
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var id SubmissionID
			sub := w.Submission
			row := tx.QueryRowContext(ctx, `SELECT id FROM submissions
				WHERE conversation_id=? AND request_id=? AND request_id <> ''`, sub.ConversationID, sub.RequestID)
			if err := row.Scan(&id); err == nil && id != sub.ID {
				return ErrDuplicateRequest
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return errors.New("cannot change a submission's conversation or request ID")
		}
	}
	return nil
}

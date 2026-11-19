package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrHistoryUnavailable means a document does not retain historical content.
var ErrHistoryUnavailable = errors.New("store: document retains only its latest value")

const documentColumns = "r.id,r.kind,r.key,r.scope_kind,r.scope_id,r.history,r.fork,r.created_at,r.retired_at"

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type documentRow struct {
	document Document
	scopeID  int64
}

func (d *documentRow) destinations() []any {
	doc := &d.document
	return []any{&doc.ID, &doc.Kind, &doc.Key, &doc.Scope.Kind, &d.scopeID, &doc.History, &doc.Fork, &doc.CreatedAt, &doc.RetiredAt}
}

func (d *documentRow) decode() Document {
	switch d.document.Scope.Kind {
	case "conversation":
		d.document.Scope.ConversationID = ConversationID(d.scopeID)
	case "task":
		d.document.Scope.TaskID = TaskID(d.scopeID)
	}
	return d.document
}

func readDocument(row scanner) (Document, error) {
	var d documentRow
	if err := row.Scan(d.destinations()...); err != nil {
		return Document{}, readError(err)
	}
	return d.decode(), nil
}

func scopeID(scope Scope) (int64, error) {
	switch {
	case scope.Kind == "session" && scope.ConversationID == 0 && scope.TaskID == 0:
		return 0, nil
	case scope.Kind == "conversation" && scope.ConversationID > 0 && scope.TaskID == 0:
		return int64(scope.ConversationID), nil
	case scope.Kind == "task" && scope.TaskID > 0 && scope.ConversationID == 0:
		return int64(scope.TaskID), nil
	default:
		return 0, fmt.Errorf("store: invalid document scope %v", scope)
	}
}

func aliveAt(at Seq) (string, []any) {
	if at == Current {
		return "r.retired_at IS NULL", nil
	}
	return "r.created_at <= ? AND (r.retired_at IS NULL OR r.retired_at > ?)", []any{at, at}
}

// FindDocument resolves the incarnation of a document address at a commit.
func (s *SQLite) FindDocument(ctx context.Context, kind string, scope Scope, key string, at Seq) (Document, error) {
	id, err := scopeID(scope)
	if err != nil {
		return Document{}, err
	}
	live, args := aliveAt(at)
	return readDocument(s.db.QueryRowContext(ctx, "SELECT "+documentColumns+`
		FROM documents r WHERE r.scope_kind=? AND r.scope_id=? AND r.kind=? AND r.key=? AND `+live,
		append([]any{scope.Kind, id, kind, key}, args...)...))
}

// Documents scans document incarnations alive at the given commit.
func (s *SQLite) Documents(ctx context.Context, q DocumentQuery, scan Scan) (Page[Document], error) {
	var f conditions
	live, args := aliveAt(q.At)
	f.add(live, args...)
	if q.Scope != (Scope{}) {
		id, err := scopeID(q.Scope)
		if err != nil {
			return Page[Document]{}, err
		}
		f.add("r.scope_kind=? AND r.scope_id=?", q.Scope.Kind, id)
	}
	if q.Kind != "" {
		f.add("r.kind=?", q.Kind)
	}
	return pageRecords(ctx, s.db, "SELECT r.id,"+documentColumns+" FROM documents r", f, scan, readDocument)
}

// Document reads an incarnation and the value visible at the given commit.
func (s *SQLite) Document(ctx context.Context, id DocumentID, at Seq) (DocumentValue, error) {
	return documentAt(ctx, s.db, id, at)
}

func retainsHistory(d Document) bool {
	return d.Scope.Kind == "conversation" && d.History == "rewindable"
}

func documentAt(ctx context.Context, db rowQuerier, id DocumentID, at Seq) (DocumentValue, error) {
	live, args := aliveAt(at)
	revision := "v.document_id=r.id"
	params := []any{id}
	if at != Current {
		revision += " AND v.seq <= ?"
		params = append([]any{at}, params...)
	}
	// Use one statement so the metadata and materialized revision share a snapshot.
	query := "SELECT " + documentColumns + `,d.version,d.value
		FROM documents r LEFT JOIN document_revisions d ON d.document_id=r.id AND d.seq=(
			SELECT max(v.seq) FROM document_revisions v WHERE ` + revision + `
		) WHERE r.id=? AND ` + live
	var d documentRow
	var version sql.NullInt64
	var value []byte
	if err := db.QueryRowContext(ctx, query, append(params, args...)...).Scan(
		append(d.destinations(), &version, &value)...); err != nil {
		return DocumentValue{}, readError(err)
	}
	doc := d.decode()
	if at != Current && !retainsHistory(doc) {
		return DocumentValue{}, ErrHistoryUnavailable
	}
	if !version.Valid {
		return DocumentValue{}, fmt.Errorf("store: document %d has no revision at %d", id, at)
	}
	return DocumentValue{Document: doc, Version: int(version.Int64), Value: value}, nil
}

type documentAction struct {
	content *Write
	retire  bool
}

func writeDocuments(ctx context.Context, tx *sql.Tx, seq Seq, writes []Write) error {
	if len(writes) == 0 {
		return nil
	}
	actions := make(map[DocumentID]documentAction)
	var order []DocumentID
	for _, w := range writes {
		id := w.Document.ID
		action, seen := actions[id]
		if !seen {
			order = append(order, id)
		}
		if w.Type == WriteDocumentRetire {
			if action.retire {
				return fmt.Errorf("store: document %d retired multiple times", id)
			}
			action.retire = true
		} else {
			if action.content != nil {
				return fmt.Errorf("store: document %d has multiple content writes", id)
			}
			action.content = &w
		}
		actions[id] = action
	}
	// Copies see the pre-batch source. Apply retirements before creations so a
	// batch can atomically replace the current incarnation of an address.
	copies := make(map[DocumentID]json.RawMessage)
	for _, id := range order {
		if w := actions[id].content; w != nil && w.Type == WriteDocumentCopy {
			source, err := documentAt(ctx, tx, *w.CopyFrom, Current)
			if err != nil {
				return fmt.Errorf("store: document %d copy source: %w", id, err)
			}
			copies[id] = source.Value
		}
	}
	for _, id := range order {
		action := actions[id]
		if w := action.content; w != nil && w.Type != WriteDocumentChange {
			continue
		}
		if err := updateDocument(ctx, tx, id, seq, action); err != nil {
			return fmt.Errorf("store: update document %d: %w", id, err)
		}
	}
	for _, id := range order {
		action := actions[id]
		w := action.content
		if w == nil || w.Type == WriteDocumentChange {
			continue
		}
		value := w.Value
		if w.Type == WriteDocumentCopy {
			value = copies[id]
		}
		if err := createDocument(ctx, tx, *w.Document, seq, action.retire, value); err != nil {
			return fmt.Errorf("store: create document %d: %w", id, err)
		}
	}
	return nil
}

func createDocument(ctx context.Context, tx *sql.Tx, doc Document, seq Seq, retire bool, value json.RawMessage) error {
	scope, err := scopeID(doc.Scope)
	if err != nil {
		return err
	}
	if doc.Kind == "" {
		return errors.New("document kind is required")
	}
	if err := claimID(ctx, tx, int64(doc.ID), "document"); err != nil {
		return err
	}
	var retiredAt *Seq
	if retire {
		retiredAt = &seq
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO documents VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.ID, doc.Kind, doc.Key, doc.Scope.Kind, scope, doc.History, doc.Fork, seq, retiredAt); err != nil {
		return err
	}
	return insertRevision(ctx, tx, doc.ID, seq, 1, value)
}

func updateDocument(ctx context.Context, tx *sql.Tx, id DocumentID, seq Seq, action documentAction) error {
	current, err := documentAt(ctx, tx, id, Current)
	if err != nil {
		return err
	}
	if w := action.content; w != nil {
		value, err := changeValue(current.Value, w.Value, w.Ops)
		if err != nil {
			return err
		}
		if !retainsHistory(current.Document) {
			if _, err := tx.ExecContext(ctx, "DELETE FROM document_revisions WHERE document_id=?", id); err != nil {
				return err
			}
		}
		if err := insertRevision(ctx, tx, id, seq, current.Version+1, value); err != nil {
			return err
		}
	}
	if action.retire {
		_, err = tx.ExecContext(ctx, "UPDATE documents SET retired_at=? WHERE id=?", seq, id)
	}
	return err
}

func insertRevision(ctx context.Context, tx *sql.Tx, id DocumentID, seq Seq, version int, value json.RawMessage) error {
	if !json.Valid(value) || firstByte(value) != '{' {
		return errors.New("document value must be a JSON object")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO document_revisions VALUES (?, ?, ?, ?)", id, seq, version, string(value))
	return err
}

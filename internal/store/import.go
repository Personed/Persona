package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
)

// ErrNotEmpty is returned when an import target already holds records.
var ErrNotEmpty = errors.New("store: import target is not empty")

// ImportReport counts imported rows. Unmodeled lists records whose JSON has fields
// the Go types do not read; their record column still keeps the original JSON.
type ImportReport struct {
	Conversations, Entries, Tasks, Submissions, Documents, Revisions int
	Unmodeled                                                        []string
}

// ImportPiDurable copies a pi-durable SQLite database (schema 1 or 2) into this empty
// store in one transaction. IDs, sequences, record JSON and document versions keep
// their pi-durable values, so pi-durable seqs stay valid read points. Revisions are
// materialized; documents without rewindable history keep only their latest value.
func (s *SQLite) ImportPiDurable(ctx context.Context, src *sql.DB) (ImportReport, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	if s.closed {
		return ImportReport{}, ErrClosed
	}
	in, err := src.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ImportReport{}, err
	}
	defer func() { _ = in.Rollback() }()
	var version sql.NullInt64
	if err := in.QueryRowContext(ctx, "SELECT max(version) FROM durable_schema").Scan(&version); err != nil {
		return ImportReport{}, fmt.Errorf("store: not a pi-durable database: %w", err)
	}
	if version.Int64 != 1 && version.Int64 != 2 {
		return ImportReport{}, fmt.Errorf("store: unsupported pi-durable schema version %d", version.Int64)
	}
	var nextID string
	var nextSeq int64
	if err := in.QueryRowContext(ctx, "SELECT next_id, next_seq FROM durable_metadata WHERE singleton = 1").Scan(&nextID, &nextSeq); err != nil {
		return ImportReport{}, fmt.Errorf("store: read pi-durable metadata: %w", err)
	}
	lastID, err := strconv.ParseInt(nextID, 10, 64)
	if err != nil {
		return ImportReport{}, fmt.Errorf("store: pi-durable next_id %q: %w", nextID, err)
	}
	im := importer{ctx: ctx, in: in}
	err = transaction(ctx, s.db, func(tx *sql.Tx) error {
		im.out = tx
		var seq Seq
		var id int64
		if err := tx.QueryRowContext(ctx, "SELECT seq, last_id FROM store_state").Scan(&seq, &id); err != nil {
			return err
		}
		if seq != 0 || id != 0 {
			return ErrNotEmpty
		}
		for _, step := range []func() error{im.recordIDs, im.conversations, im.entries, im.tasks, im.submissions, im.documents} {
			if err := step(); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE store_state SET seq=?, last_id=? WHERE singleton=1", nextSeq-1, lastID-1)
		return err
	})
	if err != nil {
		return ImportReport{}, err
	}
	s.publish(Seq(nextSeq - 1))
	return im.report, nil
}

type importer struct {
	ctx    context.Context
	in     *sql.Tx
	out    *sql.Tx
	report ImportReport
}

func (im *importer) rows(query string, each func(scan func(...any) error) error, args ...any) error {
	rows, err := im.in.QueryContext(im.ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := each(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// decode reads a record into v and notes it when v does not keep every field.
func (im *importer) decode(name string, id int64, record []byte, v any) error {
	if err := json.Unmarshal(record, v); err != nil {
		return fmt.Errorf("store: import %s %d: %w", name, id, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if !sameJSON(record, out) {
		im.report.Unmodeled = append(im.report.Unmodeled, fmt.Sprintf("%s %d", name, id))
	}
	return nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	for _, p := range []struct {
		data []byte
		v    *any
	}{{a, &x}, {b, &y}} {
		d := json.NewDecoder(bytes.NewReader(p.data))
		d.UseNumber()
		if d.Decode(p.v) != nil {
			return false
		}
	}
	return reflect.DeepEqual(x, y)
}

func (im *importer) exec(query string, args ...any) error {
	_, err := im.out.ExecContext(im.ctx, query, args...)
	return err
}

func (im *importer) recordIDs() error {
	return im.rows("SELECT id, record_type FROM record_ids ORDER BY id", func(scan func(...any) error) error {
		var id int64
		var kind string
		if err := scan(&id, &kind); err != nil {
			return err
		}
		return im.exec("INSERT INTO record_ids VALUES (?, ?)", id, kind)
	})
}

func (im *importer) conversations() error {
	return im.rows("SELECT id, record FROM conversations ORDER BY id", func(scan func(...any) error) error {
		var id int64
		var record []byte
		var c Conversation
		if err := scan(&id, &record); err != nil {
			return err
		}
		if err := im.decode("conversation", id, record, &c); err != nil {
			return err
		}
		var parent ForkPoint
		var owner TaskOwner
		if c.Parent != nil {
			parent = *c.Parent
		}
		if c.Owner != nil {
			owner = *c.Owner
		}
		im.report.Conversations++
		return im.exec("INSERT INTO conversations VALUES (?, ?, ?, ?, ?, ?)",
			id, parent.ConversationID, parent.At, owner.ConversationID, owner.TaskID, string(record))
	})
}

func (im *importer) entries() error {
	return im.rows("SELECT id, commit_seq, record FROM entries ORDER BY id", func(scan func(...any) error) error {
		var id, seq int64
		var record []byte
		var e Entry
		if err := scan(&id, &seq, &record); err != nil {
			return err
		}
		if err := im.decode("entry", id, record, &e); err != nil {
			return err
		}
		im.report.Entries++
		return im.exec("INSERT INTO entries VALUES (?, ?, ?, ?, ?)", id, e.ConversationID, seq, e.Head, string(record))
	})
}

func (im *importer) tasks() error {
	return im.rows("SELECT id, record FROM tasks ORDER BY id", func(scan func(...any) error) error {
		var id int64
		var record []byte
		var t Task
		if err := scan(&id, &record); err != nil {
			return err
		}
		if err := im.decode("task", id, record, &t); err != nil {
			return err
		}
		im.report.Tasks++
		return im.exec("INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?, ?)",
			id, t.ConversationID, t.Kind, t.State.Status, t.AbortRequested, t.Background, string(record))
	})
}

func (im *importer) submissions() error {
	return im.rows("SELECT id, record FROM submissions ORDER BY id", func(scan func(...any) error) error {
		var id int64
		var record []byte
		var sub Submission
		if err := scan(&id, &record); err != nil {
			return err
		}
		if err := im.decode("submission", id, record, &sub); err != nil {
			return err
		}
		im.report.Submissions++
		return im.exec("INSERT INTO submissions VALUES (?, ?, ?, ?, ?)",
			id, sub.ConversationID, sub.RequestID, sub.Status, string(record))
	})
}

func (im *importer) documents() error {
	return im.rows("SELECT id, record FROM documents ORDER BY id", func(scan func(...any) error) error {
		var id int64
		var record []byte
		var d Document
		if err := scan(&id, &record); err != nil {
			return err
		}
		if err := im.decode("document", id, record, &d); err != nil {
			return err
		}
		scope, err := scopeID(d.Scope)
		if err != nil {
			return fmt.Errorf("store: import document %d: %w", id, err)
		}
		if err := im.exec("INSERT INTO documents VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			id, d.Kind, d.Key, d.Scope.Kind, scope, d.History, d.Fork, d.CreatedAt, d.RetiredAt); err != nil {
			return fmt.Errorf("store: import document %d: %w", id, err)
		}
		im.report.Documents++
		return im.revisions(d)
	})
}

// revisions replays pi-durable's base and delta rows into materialized values.
func (im *importer) revisions(d Document) error {
	type revision struct {
		seq     Seq
		version int
		value   json.RawMessage
	}
	var all []revision
	err := im.rows("SELECT seq, kind, version, content FROM document_revisions WHERE document_id = ? ORDER BY seq",
		func(scan func(...any) error) error {
			var r revision
			var kind string
			var content []byte
			if err := scan(&r.seq, &kind, &r.version, &content); err != nil {
				return err
			}
			switch {
			case kind == "base":
				r.value = content
			case len(all) == 0:
				return fmt.Errorf("store: import document %d: delta at %d has no base", d.ID, r.seq)
			case all[len(all)-1].version != r.version:
				return fmt.Errorf("store: import document %d: delta at %d crosses a version", d.ID, r.seq)
			default:
				value, err := applyDelta(all[len(all)-1].value, content)
				if err != nil {
					return fmt.Errorf("store: import document %d delta at %d: %w", d.ID, r.seq, err)
				}
				r.value = value
			}
			all = append(all, r)
			return nil
		}, d.ID)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		if d.RetiredAt == nil {
			return fmt.Errorf("store: import document %d: no revisions", d.ID)
		}
		return nil
	}
	if !retainsHistory(d) {
		all = all[len(all)-1:]
	}
	for _, r := range all {
		if err := insertRevision(im.ctx, im.out, d.ID, r.seq, r.version, r.value); err != nil {
			return fmt.Errorf("store: import document %d at %d: %w", d.ID, r.seq, err)
		}
		im.report.Revisions++
	}
	return nil
}

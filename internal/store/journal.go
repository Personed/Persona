package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// CallState is the durable phase of a tool call.
type CallState string

const (
	// CallIntent is recorded before the first dispatch.
	CallIntent CallState = "intent"
	// CallComplete has a saved result, including results that report tool errors.
	CallComplete CallState = "complete"
	// CallUncertain was interrupted without a saved result and must not be replayed.
	CallUncertain CallState = "uncertain"
)

// CallRecord retains a tool's original intent and, when known, its complete result.
type CallRecord struct {
	TaskID TaskID
	CallID string
	State  CallState
	Intent json.RawMessage
	Result json.RawMessage
}

// BeginCall records intent before dispatch. Only a true return authorizes
// dispatch. Repeated task/call IDs return the existing record without authorizing
// dispatch, even if it has no result. Use the same ID for every retry.
func (s *SQLite) BeginCall(ctx context.Context, taskID TaskID, callID string, intent json.RawMessage) (record CallRecord, dispatch bool, err error) {
	if taskID <= 0 || callID == "" || !json.Valid(intent) {
		return CallRecord{}, false, errors.New("store: call requires a task ID, call ID and JSON intent")
	}
	err = s.journalWrite(ctx, func(tx *sql.Tx) (bool, error) {
		result, err := tx.ExecContext(ctx, `INSERT INTO call_journal(task_id,call_id,state,intent) VALUES (?,?,'intent',?)
			ON CONFLICT(task_id,call_id) DO NOTHING`, taskID, callID, string(intent))
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		dispatch = n == 1
		record, err = loadCall(ctx, tx, taskID, callID)
		return dispatch, err
	})
	if err != nil {
		return CallRecord{}, false, err
	}
	return record, dispatch, nil
}

// CompleteCall stores a confirmed result. Repeating the same result is safe; a
// different result cannot overwrite it. Recovery may settle an uncertain call
// with verified evidence, but must not dispatch it again to obtain a result.
func (s *SQLite) CompleteCall(ctx context.Context, taskID TaskID, callID string, result json.RawMessage) error {
	if !json.Valid(result) {
		return errors.New("store: call result must be JSON")
	}
	return s.journalWrite(ctx, func(tx *sql.Tx) (bool, error) {
		updated, err := tx.ExecContext(ctx, `UPDATE call_journal SET state='complete',result=?
			WHERE task_id=? AND call_id=? AND state <> 'complete'`, string(result), taskID, callID)
		if err != nil {
			return false, err
		}
		n, err := updated.RowsAffected()
		if err != nil || n == 1 {
			return n == 1, err
		}
		record, err := loadCall(ctx, tx, taskID, callID)
		if err != nil {
			return false, err
		}
		var saved, incoming bytes.Buffer
		if err := json.Compact(&saved, record.Result); err != nil {
			return false, err
		}
		if err := json.Compact(&incoming, result); err != nil {
			return false, err
		}
		if !bytes.Equal(saved.Bytes(), incoming.Bytes()) {
			return false, errors.New("store: call already has a different result")
		}
		return false, nil
	})
}

// Calls lists the journal for one task, ordered by call ID. In-progress or
// uncertain records are evidence of admission, not a list of calls to replay.
func (s *SQLite) Calls(ctx context.Context, taskID TaskID) ([]CallRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id,call_id,state,intent,result
		FROM call_journal WHERE task_id=? ORDER BY call_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []CallRecord
	for rows.Next() {
		record, err := readCall(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func loadCall(ctx context.Context, db rowQuerier, taskID TaskID, callID string) (CallRecord, error) {
	return readCall(db.QueryRowContext(ctx, `SELECT task_id,call_id,state,intent,result
		FROM call_journal WHERE task_id=? AND call_id=?`, taskID, callID))
}

func readCall(row scanner) (CallRecord, error) {
	var r CallRecord
	var intent, result []byte
	if err := row.Scan(&r.TaskID, &r.CallID, &r.State, &intent, &result); err != nil {
		return CallRecord{}, readError(err)
	}
	r.Intent, r.Result = intent, result
	return r, nil
}

// RecoveryTasks lists nonterminal tasks with their original checkpoints and
// states. The runtime must reconcile running/completing work against the engine
// and call journal; storage never restarts a task or repeats a tool call.
// Enumerate it on startup before admitting new work.
func (s *SQLite) RecoveryTasks(ctx context.Context, scan Scan) (Page[Task], error) {
	var f conditions
	f.add("status <> 'terminal'")
	return pageRecords(ctx, s.db, "SELECT r.id,r.record FROM tasks r", f, scan, readRecord[Task])
}

func (s *SQLite) recoverInterruptedCalls(ctx context.Context) error {
	return s.journalWrite(ctx, func(tx *sql.Tx) (bool, error) {
		result, err := tx.ExecContext(ctx, "UPDATE call_journal SET state='uncertain' WHERE state='intent'")
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		return n != 0, err
	})
}

func (s *SQLite) journalWrite(ctx context.Context, write func(*sql.Tx) (bool, error)) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	if s.closed {
		return ErrClosed
	}
	var seq Seq
	err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		changed, err := write(tx)
		if err != nil || !changed {
			return err
		}
		seq, err = nextSeq(ctx, tx)
		return err
	})
	if err == nil && seq != 0 {
		s.publish(seq)
	}
	return err
}

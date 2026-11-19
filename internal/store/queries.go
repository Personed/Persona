package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

const maxPage = 200

type scanner interface {
	Scan(...any) error
}

func readRecord[T any](row scanner) (value T, err error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		return value, readError(err)
	}
	err = json.Unmarshal(body, &value)
	return value, err
}

func readError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

type conditions struct {
	where []string
	args  []any
}

func (f *conditions) add(clause string, args ...any) {
	f.where = append(f.where, clause)
	f.args = append(f.args, args...)
}

func pageRecords[T any](ctx context.Context, db *sql.DB, selectSQL string, f conditions, scan Scan, decode func(scanner) (T, error)) (Page[T], error) {
	if scan.Limit < 0 {
		return Page[T]{}, errors.New("store: negative page limit")
	}
	limit := min(scan.Limit, maxPage)
	if limit == 0 {
		limit = maxPage
	}
	order, compare, prefix := "ASC", ">", "a:"
	if scan.Descending {
		order, compare, prefix = "DESC", "<", "d:"
	}
	if scan.Cursor != "" {
		suffix, ok := strings.CutPrefix(scan.Cursor, prefix)
		id, err := strconv.ParseInt(suffix, 10, 64)
		if !ok || err != nil || id <= 0 {
			return Page[T]{}, errors.New("store: invalid cursor for scan order")
		}
		f.add("r.id "+compare+" ?", id)
	}
	if len(f.where) != 0 {
		selectSQL += " WHERE " + strings.Join(f.where, " AND ")
	}
	rows, err := db.QueryContext(ctx, selectSQL+" ORDER BY r.id "+order+" LIMIT ?", append(f.args, limit+1)...)
	if err != nil {
		return Page[T]{}, err
	}
	defer func() { _ = rows.Close() }()
	page := Page[T]{Items: make([]T, 0, limit)}
	var last int64
	for rows.Next() {
		if len(page.Items) == limit {
			page.Next = prefix + strconv.FormatInt(last, 10)
			break
		}
		item, err := decode(withID{row: rows, id: &last})
		if err != nil {
			return Page[T]{}, err
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

type withID struct {
	row scanner
	id  *int64
}

func (w withID) Scan(dest ...any) error {
	return w.row.Scan(append([]any{w.id}, dest...)...)
}

// Conversation reads a conversation by ID.
func (s *SQLite) Conversation(ctx context.Context, id ConversationID) (Conversation, error) {
	return readRecord[Conversation](s.db.QueryRowContext(ctx, "SELECT record FROM conversations WHERE id=?", id))
}

// Conversations scans conversations, optionally filtered by task owner.
func (s *SQLite) Conversations(ctx context.Context, q ConversationQuery, scan Scan) (Page[Conversation], error) {
	var f conditions
	if q.OwnerConversationID != 0 {
		f.add("owner_conversation_id=?", q.OwnerConversationID)
	}
	if q.OwnerTaskID != 0 {
		f.add("owner_task_id=?", q.OwnerTaskID)
	}
	return pageRecords(ctx, s.db, "SELECT r.id, r.record FROM conversations r", f, scan, readRecord[Conversation])
}

// Entry reads an immutable entry and the sequence at which it was committed.
func (s *SQLite) Entry(ctx context.Context, id EntryID) (Entry, Seq, error) {
	var body []byte
	var seq Seq
	if err := s.db.QueryRowContext(ctx, "SELECT record,seq FROM entries WHERE id=?", id).Scan(&body, &seq); err != nil {
		return Entry{}, 0, readError(err)
	}
	var entry Entry
	err := json.Unmarshal(body, &entry)
	return entry, seq, err
}

// UNION terminates even if an imported fork graph contains a cycle. The maximum
// across visits keeps each ancestor once, subject to the earliest cutoff on its path.
const lineage = `WITH RECURSIVE lineage(id, cutoff) AS (
		SELECT ?, ?
		UNION
		SELECT c.parent_id, min(l.cutoff, c.fork_at) FROM lineage l JOIN conversations c ON c.id=l.id
		WHERE c.parent_id <> 0
	), bounds AS (SELECT id, max(cutoff) AS cutoff FROM lineage GROUP BY id)`

const lineageJoin = " JOIN bounds b ON r.conversation_id=b.id AND r.id <= b.cutoff"

func endEntryID(id EntryID) EntryID {
	if id == 0 {
		return math.MaxInt64
	}
	return id
}

// LatestHead finds the most recent visible head marker at or before the given ID.
func (s *SQLite) LatestHead(ctx context.Context, conversation ConversationID, atOrBefore EntryID) (Entry, error) {
	return readRecord[Entry](s.db.QueryRowContext(ctx, lineage+`
		SELECT r.record FROM entries r`+lineageJoin+`
		WHERE r.head IS NOT NULL ORDER BY r.id DESC LIMIT 1`, conversation, endEntryID(atOrBefore)))
}

// Entries scans entries, respecting fork points when given a conversation.
func (s *SQLite) Entries(ctx context.Context, q EntryQuery, scan Scan) (Page[Entry], error) {
	var f conditions
	query := "SELECT r.id, r.record FROM entries r"
	if q.ConversationID != 0 {
		f.args = append(f.args, q.ConversationID, endEntryID(q.Max))
		query = lineage + query + lineageJoin
	} else if q.Max != 0 {
		f.add("r.id <= ?", q.Max)
	}
	if q.Min != 0 {
		f.add("r.id >= ?", q.Min)
	}
	return pageRecords(ctx, s.db, query, f, scan, readRecord[Entry])
}

// Task reads a task and its stored state by ID.
func (s *SQLite) Task(ctx context.Context, id TaskID) (Task, error) {
	return readRecord[Task](s.db.QueryRowContext(ctx, "SELECT record FROM tasks WHERE id=?", id))
}

// Tasks scans tasks matching the query.
func (s *SQLite) Tasks(ctx context.Context, q TaskQuery, scan Scan) (Page[Task], error) {
	var f conditions
	if q.ConversationID != 0 {
		f.add("conversation_id=?", q.ConversationID)
	}
	if q.Kind != "" {
		f.add("kind=?", q.Kind)
	}
	if q.Status != "" {
		f.add("status=?", q.Status)
	}
	if q.AbortRequested != nil {
		f.add("abort_requested=?", *q.AbortRequested)
	}
	if q.Background != nil {
		f.add("background=?", *q.Background)
	}
	return pageRecords(ctx, s.db, "SELECT r.id, r.record FROM tasks r", f, scan, readRecord[Task])
}

// Submission reads an admitted input by ID.
func (s *SQLite) Submission(ctx context.Context, id SubmissionID) (Submission, error) {
	return readRecord[Submission](s.db.QueryRowContext(ctx, "SELECT record FROM submissions WHERE id=?", id))
}

// SubmissionByRequest looks up an admitted input by its nonempty request ID.
func (s *SQLite) SubmissionByRequest(ctx context.Context, conversation ConversationID, requestID string) (Submission, error) {
	return readRecord[Submission](s.db.QueryRowContext(ctx, `SELECT record FROM submissions
		WHERE conversation_id=? AND request_id=? AND request_id <> ''`, conversation, requestID))
}

// Submissions scans admitted inputs matching the query.
func (s *SQLite) Submissions(ctx context.Context, q SubmissionQuery, scan Scan) (Page[Submission], error) {
	var f conditions
	if q.ConversationID != 0 {
		f.add("conversation_id=?", q.ConversationID)
	}
	if q.Status != "" {
		f.add("status=?", q.Status)
	}
	return pageRecords(ctx, s.db, "SELECT r.id, r.record FROM submissions r", f, scan, readRecord[Submission])
}

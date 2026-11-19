package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// openPiFixture loads testdata/pidurable/fixture.sql, written by the installed pi-durable.
func openPiFixture(t *testing.T) *sql.DB {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("testdata", "pidurable", "fixture.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(filepath.Join(t.TempDir(), "agent.sqlite"), "rwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), string(script)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestImportPiDurableKeepsEveryRecord(t *testing.T) {
	ctx := t.Context()
	src := openPiFixture(t)
	s, _ := openTestStore(t)
	report, err := s.ImportPiDurable(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unmodeled) != 0 || report.Conversations != 3 || report.Entries != 2 || report.Tasks != 1 ||
		report.Submissions != 2 || report.Documents != 6 {
		t.Fatalf("report = %+v", report)
	}

	reads := map[string]func(id int64) (any, error){
		"conversations": func(id int64) (any, error) { return s.Conversation(ctx, ConversationID(id)) },
		"entries":       func(id int64) (any, error) { e, _, err := s.Entry(ctx, EntryID(id)); return e, err },
		"tasks":         func(id int64) (any, error) { return s.Task(ctx, TaskID(id)) },
		"submissions":   func(id int64) (any, error) { return s.Submission(ctx, SubmissionID(id)) },
	}
	for table, read := range reads {
		rows, err := src.QueryContext(ctx, "SELECT id, record FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			var want []byte
			if err := rows.Scan(&id, &want); err != nil {
				t.Fatal(err)
			}
			record, err := read(id)
			if err != nil {
				t.Fatalf("%s %d: %v", table, id, err)
			}
			got, _ := json.Marshal(record)
			if !sameJSON(got, want) {
				t.Errorf("%s %d = %s, want %s", table, id, got, want)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, seq, err := s.Entry(ctx, 8); err != nil || seq != 3 {
		t.Errorf("entry 8 commit seq = %d, %v; want pi-durable's 3", seq, err)
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "pidurable", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		LastSeq   Seq `json:"lastSeq"`
		Documents map[DocumentID]struct {
			Current json.RawMessage            `json:"current"`
			At      map[string]json.RawMessage `json:"at"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	check := func(id DocumentID, at Seq, want json.RawMessage) {
		t.Helper()
		got, err := s.Document(ctx, id, at)
		switch string(want) {
		case "null":
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("document %d at %d = %s, %v; want not found", id, at, got.Value, err)
			}
		case `"unavailable"`:
			if !errors.Is(err, ErrHistoryUnavailable) && !errors.Is(err, ErrNotFound) {
				t.Errorf("document %d at %d = %s, %v; want no history", id, at, got.Value, err)
			}
		default:
			value, _ := json.Marshal(map[string]any{"version": got.Version, "value": got.Value})
			if err != nil || !sameJSON(value, want) {
				t.Errorf("document %d at %d = %s, %v; want %s", id, at, value, err, want)
			}
		}
	}
	for id, doc := range expected.Documents {
		check(id, Current, doc.Current)
		for at, want := range doc.At {
			seq, err := strconv.ParseInt(at, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			check(id, Seq(seq), want)
		}
	}
	notes, err := s.FindDocument(ctx, "notes", Scope{Kind: "conversation", ConversationID: 2}, "weekly", Current)
	if err != nil || notes.ID != 15 {
		t.Errorf("current notes = %+v, %v; want document 15", notes, err)
	}

	if _, err := s.ImportPiDurable(ctx, src); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("second import = %v, want ErrNotEmpty", err)
	}
	if id, err := s.MintID(ctx); err != nil || id != 16 {
		t.Errorf("MintID after import = %d, %v; want pi-durable's next_id 16", id, err)
	}
	if seq := mustCommit(t, s, conversationWrite(17)); seq != expected.LastSeq+1 {
		t.Errorf("first commit after import = %d, want %d", seq, expected.LastSeq+1)
	}
}

func TestImportPiDurableRefusesWhatItCannotKeep(t *testing.T) {
	ctx := t.Context()
	for name, c := range map[string]struct {
		edit  string
		watch bool
		want  string
	}{
		"explicit empty key": {edit: `UPDATE documents SET family=1, record=json_set(record,'$.key','') WHERE id=4`, want: "document 4"},
		"unknown field":      {edit: `UPDATE documents SET record=json_set(record,'$.origin','legacy') WHERE id=14`, want: "document 14"},
		"watched store":      {watch: true, want: "not watched"},
	} {
		t.Run(name, func(t *testing.T) {
			src := openPiFixture(t)
			if c.edit != "" {
				if _, err := src.ExecContext(ctx, c.edit); err != nil {
					t.Fatal(err)
				}
			}
			s, _ := openTestStore(t)
			if c.watch {
				if _, err := s.Watch(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ImportPiDurable(ctx, src); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("import = %v, want error naming %q", err, c.want)
			}
			if id, err := s.MintID(ctx); err != nil || id != 1 {
				t.Fatalf("store changed after refused import: MintID = %d, %v", id, err)
			}
		})
	}
}

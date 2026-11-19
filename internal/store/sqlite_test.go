package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "persona #%.sqlite")
	return openTestPath(t, path), path
}

func openTestPath(t *testing.T, path string) *SQLite {
	t.Helper()
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func mustCommit(t *testing.T, s Store, batch ...Write) Seq {
	t.Helper()
	n, seqs, err := s.Commit(t.Context(), []Batch{batch})
	if err != nil || n != 1 || len(seqs) != 1 {
		t.Fatalf("Commit = %d, %v, %v", n, seqs, err)
	}
	return seqs[0]
}

func conversationWrite(id ConversationID) Write {
	return Write{Type: WriteConversation, Conversation: &Conversation{ID: id}}
}

func TestRuntimeLeaseAndWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persona.sqlite")
	ctx, cancel := context.WithCancel(t.Context())
	s, err := Open(ctx, path)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var mode string
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, %v", mode, err)
	}
	var synchronous int
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("synchronous = %d, %v", synchronous, err)
	}
	if other, err := Open(t.Context(), filepath.Join(dir, "other.sqlite")); !errors.Is(err, ErrInUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("another runtime in the same directory: %v", err)
	}
	legacyLease, err := openSQLite(filepath.Join(dir, "runtime.sqlite"), "rwc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacyLease.Close() }()
	if _, err := legacyLease.ExecContext(t.Context(), "PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE"); err == nil {
		t.Fatal("legacy runtime acquired the directory while Go held it")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyLease.ExecContext(t.Context(), "PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("legacy runtime could not acquire lease after Go closed: %v", err)
	}
	if other, err := Open(t.Context(), path); !errors.Is(err, ErrInUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("Go runtime opened under a legacy lease: %v", err)
	}
}

func TestOpenRefusesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := openSQLite(path, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE durable_schema (version INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(t.Context(), path); err == nil {
		_ = s.Close()
		t.Fatal("Open accepted an existing legacy database")
	}
	var mode string
	if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("existing database changed journal mode: %q, %v", mode, err)
	}
	// A failed Open must release the directory lease.
	openTestPath(t, filepath.Join(filepath.Dir(path), "persona.sqlite"))
}

func TestCommitKeepsAcceptedPrefix(t *testing.T) {
	s, path := openTestStore(t)
	ctx := t.Context()
	entry := Write{Type: WriteEntry, Entry: &Entry{ID: 2, ConversationID: 1, Kind: "input"}}
	n, seqs, err := s.Commit(ctx, []Batch{
		{conversationWrite(1), entry},
		{conversationWrite(100), entry},
		{conversationWrite(300)},
	})
	if err == nil || n != 1 || !slices.Equal(seqs, []Seq{1}) {
		t.Fatalf("Commit = %d, %v, %v", n, seqs, err)
	}
	for _, id := range []ConversationID{100, 300} {
		if _, err := s.Conversation(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("uncommitted conversation %d: %v", id, err)
		}
	}
	if _, _, err := s.Commit(ctx, []Batch{{conversationWrite(2)}}); err == nil {
		t.Fatal("entry ID accepted for a conversation")
	}
	if id, err := s.MintID(ctx); id != 3 || err != nil {
		t.Fatalf("MintID = %d, %v", id, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestPath(t, path)
	got, seq, err := s.Entry(ctx, 2)
	if err != nil || seq != 1 || got.Kind != "input" {
		t.Fatalf("committed entry = %+v at %d, %v", got, seq, err)
	}
	if id, err := s.MintID(ctx); id != 4 || err != nil {
		t.Fatalf("MintID after reopen = %d, %v", id, err)
	}
	if seq := mustCommit(t, s, conversationWrite(90)); seq != 2 {
		t.Fatalf("sequence of next committed batch = %d", seq)
	}
	if id, err := s.MintID(ctx); id != 91 || err != nil {
		t.Fatalf("MintID after an explicit imported ID = %d, %v", id, err)
	}
}

func TestWatchOrderingAndCancellation(t *testing.T) {
	s, _ := openTestStore(t)
	mustCommit(t, s, conversationWrite(1))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	first, err := s.Watch(firstCtx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			n, _, err := s.Commit(ctx, []Batch{{conversationWrite(ConversationID(i + 2))}})
			if n != 1 || err != nil {
				t.Errorf("Commit = %d, %v", n, err)
			}
		})
	}
	// A full backlog must not block commits even while nobody reads either watch.
	wg.Wait()
	for _, ch := range []<-chan Seq{first, second} {
		for i := range writers {
			select {
			case seq := <-ch:
				if seq != Seq(i+2) {
					t.Fatalf("watch received %d, expected %d", seq, i+2)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	cancelFirst()
	requireClosed(ctx, t, first)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireClosed(ctx, t, second)
}

func requireClosed(ctx context.Context, t *testing.T, ch <-chan Seq) {
	t.Helper()
	select {
	case seq, ok := <-ch:
		if ok {
			t.Fatalf("watch still open, received %d", seq)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestEntryPaginationAcrossFork(t *testing.T) {
	s, _ := openTestStore(t)
	fork := &Conversation{ID: 208, Parent: &ForkPoint{ConversationID: 1, At: 204}, Owner: &TaskOwner{ConversationID: 1, TaskID: 209}}
	batch := Batch{conversationWrite(1), {Type: WriteConversation, Conversation: fork}}
	for id := EntryID(2); id <= 207; id++ {
		e := &Entry{ID: id, ConversationID: 1}
		if id == 4 || id == 206 {
			e.Head = &id
		}
		batch = append(batch, Write{Type: WriteEntry, Entry: e})
	}
	batch = append(batch, Write{Type: WriteEntry, Entry: &Entry{ID: 210, ConversationID: 208}})
	mustCommit(t, s, batch...)

	ctx := t.Context()
	q := EntryQuery{ConversationID: fork.ID}
	scan := Scan{Limit: 500, Descending: true}
	p, err := s.Entries(ctx, q, scan)
	if err != nil || len(p.Items) != 200 || p.Next == "" || p.Items[0].ID != 210 || p.Items[1].ID != 204 {
		t.Fatalf("first page size=%d, next=%q, err=%v", len(p.Items), p.Next, err)
	}
	scan.Cursor = p.Next
	rest, err := s.Entries(ctx, q, scan)
	if err != nil || rest.Next != "" || len(rest.Items) != 4 || rest.Items[0].ID != p.Items[199].ID-1 || rest.Items[3].ID != 2 {
		t.Fatalf("last page = %+v, %v", rest, err)
	}
	if _, err := s.Entries(ctx, q, Scan{Cursor: p.Next}); err == nil {
		t.Fatal("accepted a descending cursor in an ascending scan")
	}
	p, err = s.Entries(ctx, EntryQuery{ConversationID: fork.ID, Min: 203, Max: 205}, Scan{Limit: 2})
	if err != nil || len(p.Items) != 2 || p.Items[0].ID != 203 || p.Items[1].ID != 204 || p.Next != "" {
		t.Fatalf("bounded ascending page = %+v, %v", p, err)
	}
	head, err := s.LatestHead(ctx, fork.ID, 0)
	if err != nil || head.ID != 4 {
		t.Fatalf("latest visible head = %+v, %v", head, err)
	}
	if _, err := s.LatestHead(ctx, fork.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head before any marker = %v", err)
	}
	owned, err := s.Conversations(ctx, ConversationQuery{OwnerConversationID: 1, OwnerTaskID: 209}, Scan{})
	if err != nil || len(owned.Items) != 1 || owned.Items[0].ID != fork.ID {
		t.Fatalf("owned conversations = %+v, %v", owned, err)
	}
}

package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestConcurrentRequestIDDeduplication(t *testing.T) {
	s, path := openTestStore(t)
	ctx := t.Context()
	const contenders = 12
	var wg sync.WaitGroup
	for id := range contenders {
		wg.Go(func() {
			entryID := EntryID(id*2 + 2)
			entry := &Entry{ID: entryID, ConversationID: 1}
			sub := &Submission{ID: SubmissionID(entry.ID + 1), ConversationID: 1, Entry: &entryID, RequestID: "retry", Status: SubmissionQueued}
			n, _, err := s.Commit(ctx, []Batch{{
				{Type: WriteEntry, Entry: entry}, {Type: WriteSubmission, Submission: sub},
			}})
			switch {
			case n == 1 && err == nil, n == 0 && errors.Is(err, ErrDuplicateRequest):
			default:
				t.Errorf("contender %d: Commit = %d, %v", sub.ID, n, err)
			}
		})
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestPath(t, path)
	admitted, err := s.SubmissionByRequest(ctx, 1, "retry")
	if err != nil || admitted.Entry == nil {
		t.Fatalf("lookup after reopening = %+v, %v", admitted, err)
	}
	entries, err := s.Entries(ctx, EntryQuery{ConversationID: 1}, Scan{})
	if err != nil || len(entries.Items) != 1 || entries.Items[0].ID != *admitted.Entry {
		t.Fatalf("concurrent retries left extra entries: %+v, %v", entries, err)
	}
	admitted.Status = SubmissionDone
	mustCommit(t, s, Write{Type: WriteSubmission, Submission: &admitted})
	mustCommit(t, s,
		Write{Type: WriteSubmission, Submission: &Submission{ID: 70, ConversationID: 2, RequestID: "retry", Status: SubmissionQueued}},
		Write{Type: WriteSubmission, Submission: &Submission{ID: 71, ConversationID: 1, Status: SubmissionQueued}},
		Write{Type: WriteSubmission, Submission: &Submission{ID: 72, ConversationID: 1, Status: SubmissionQueued}},
	)
	done, err := s.Submissions(ctx, SubmissionQuery{ConversationID: 1, Status: SubmissionDone}, Scan{})
	if err != nil || len(done.Items) != 1 || done.Items[0].ID != admitted.ID {
		t.Fatalf("settled submissions = %+v, %v", done, err)
	}
	if _, err := s.SubmissionByRequest(ctx, 1, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty request ID is not a dedupe key: %v", err)
	}
}

func TestDocumentRevisionsAndLifetimes(t *testing.T) {
	s, path := openTestStore(t)
	ctx := t.Context()
	doc := &Document{ID: 1, Kind: "notes", Key: "weekly", Scope: Scope{Kind: "conversation", ConversationID: 99}, History: "rewindable"}
	const original = `{"items":[1,2],"text":"🙂hi","remove":true,"large":9007199254740993}`
	first, err := s.CommitVersioned(ctx,
		Batch{{Type: WriteDocumentCreate, Document: doc, Value: json.RawMessage(original)}},
		map[DocumentID]int{doc.ID: 3},
	)
	if err != nil {
		t.Fatal(err)
	}
	second := mustCommit(t, s, Write{
		Type: WriteDocumentChange, Document: &Document{ID: doc.ID},
		Ops: json.RawMessage(`[["s",["done"],false],["d",["remove"]],["p",["items"],1,0,[3]],["m",["items"],[2,0,1]],["t",["text"],2],["a",["text"],"!"]]`),
	})
	const changed = `{"done":false,"items":[2,1,3],"large":9007199254740993,"text":"hi!"}`
	replacement := *doc
	replacement.ID = 2
	copyDoc := *doc
	copyDoc.ID, copyDoc.Scope.ConversationID = 3, 100
	third := mustCommit(t, s,
		Write{Type: WriteDocumentCreate, Document: &replacement, Value: json.RawMessage(`{"new":true}`)},
		Write{Type: WriteDocumentCopy, Document: &copyDoc, CopyFrom: &doc.ID},
		Write{Type: WriteDocumentRetire, Document: &Document{ID: doc.ID}},
	)
	bots := &Document{ID: 4, Kind: "persona.bots", Scope: Scope{Kind: "session"}}
	botsSeq := mustCommit(t, s, Write{Type: WriteDocumentCreate, Document: bots, Value: json.RawMessage(`{"items":[]}`)})
	mustCommit(t, s, Write{Type: WriteDocumentChange, Document: bots, Value: json.RawMessage(`{"items":[{"id":99}]}`)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestPath(t, path)

	for _, tc := range []struct {
		id      DocumentID
		at      Seq
		version int
		value   string
	}{
		{doc.ID, first, 3, original},
		{doc.ID, second, 3, changed},
		{replacement.ID, third, 1, `{"new":true}`},
		{copyDoc.ID, Current, 3, changed},
		{bots.ID, Current, 1, `{"items":[{"id":99}]}`},
	} {
		got, err := s.Document(ctx, tc.id, tc.at)
		if err != nil || got.Version != tc.version || string(got.Value) != tc.value {
			t.Errorf("document %d at %d = %+v %s, %v", tc.id, tc.at, got, got.Value, err)
		}
	}
	historical, err := s.FindDocument(ctx, doc.Kind, doc.Scope, doc.Key, second)
	if err != nil || historical.ID != doc.ID || historical.RetiredAt == nil || *historical.RetiredAt != third {
		t.Fatalf("historical address = %+v, %v", historical, err)
	}
	docs, err := s.Documents(ctx, DocumentQuery{At: Current, Scope: doc.Scope, Kind: doc.Kind}, Scan{Limit: 1})
	if err != nil || len(docs.Items) != 1 || docs.Items[0].ID != replacement.ID || docs.Next != "" {
		t.Fatalf("current scope = %+v, %v", docs, err)
	}
	if _, err := s.Document(ctx, doc.ID, third); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired doc visible on retirement commit: %v", err)
	}
	if _, err := s.Document(ctx, bots.ID, botsSeq); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("latest-only historical read: %v", err)
	}
	if _, _, err := s.Commit(ctx, []Batch{{
		{Type: WriteDocumentChange, Document: &replacement, Ops: json.RawMessage(`[["s",["new"],false],["x",["new"]]]`)},
	}}); err == nil {
		t.Fatal("invalid delta accepted")
	}
	if got, err := s.Document(ctx, replacement.ID, Current); err != nil || string(got.Value) != `{"new":true}` {
		t.Fatalf("failed operation changed original revision: %s, %v", got.Value, err)
	}
	write := Write{Type: WriteDocumentChange, Document: &replacement, Ops: json.RawMessage(`[["s",["new"],false]]`)}
	if _, err := s.CommitVersioned(ctx, Batch{write}, map[DocumentID]int{replacement.ID: 5}); err == nil {
		t.Fatal("schema change using a delta was accepted")
	}
	write.Ops, write.Value = nil, json.RawMessage(`{"migrated":true}`)
	newVersion, err := s.CommitVersioned(ctx, Batch{write}, map[DocumentID]int{replacement.ID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Document(ctx, replacement.ID, newVersion); err != nil || got.Version != 5 {
		t.Fatalf("migrated document version = %d, %v", got.Version, err)
	}
	if got, err := s.Document(ctx, replacement.ID, third); err != nil || got.Version != 1 {
		t.Fatalf("earlier revision version = %d, %v", got.Version, err)
	}
}

func TestChordDeltaPreservesUTF16AcrossCommits(t *testing.T) {
	s, path := openTestStore(t)
	doc := &Document{ID: 1, Kind: "notes", Scope: Scope{Kind: "session"}}
	mustCommit(t, s, Write{Type: WriteDocumentCreate, Document: doc, Value: json.RawMessage(`{"joined":"","split":"🙂","\ud800":"key"}`)})
	mustCommit(t, s, Write{Type: WriteDocumentChange, Document: doc,
		Ops: json.RawMessage(`[["a",["joined"],"\ud83d"],["a",["joined"],"\ude42"],["t",["split"],1],["a",["\uD800"],"!"]]`)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestPath(t, path)
	got, err := s.Document(t.Context(), doc.ID, Current)
	if err != nil || string(got.Value) != `{"\ud800":"key!","joined":"🙂","split":"\ude42"}` {
		t.Fatalf("first revision = %s, %v", got.Value, err)
	}
	mustCommit(t, s, Write{Type: WriteDocumentChange, Document: doc,
		Ops: json.RawMessage(`[["a",["split"],"\ud83d"],["a",["split"],"\ude42"]]`)})
	got, err = s.Document(t.Context(), doc.ID, Current)
	if err != nil || string(got.Value) != `{"\ud800":"key!","joined":"🙂","split":"\ude42🙂"}` {
		t.Fatalf("second revision = %s, %v", got.Value, err)
	}
}

package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/client"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

const bot = 1

// failing refuses commits while fail is set.
type failing struct {
	store.Store
	fail atomic.Bool
}

func (f *failing) Commit(ctx context.Context, batches []store.Batch) (int, []store.Seq, error) {
	if f.fail.Load() {
		return 0, nil, errors.New("disk full")
	}
	return f.Store.Commit(ctx, batches)
}

func TestFailedSettleIsRetried(t *testing.T) {
	_, records := open(t, 0)
	flaky := &failing{Store: records}
	service := approval.NewService(flaky, nil, 0)
	done := request(t.Context(), t, service, bot, approval.Request{ID: "lost", Title: "Lost"})
	flaky.fail.Store(true)
	if err := service.Answer(bot, "lost", approval.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.err == nil || result.answer.Allow {
		t.Fatalf("Request = %+v", result)
	}
	// The answered request is no longer offered, and the Bot's next request stores its outcome.
	if stored, err := service.Pending(t.Context(), bot); err != nil || len(stored) != 0 {
		t.Fatalf("Pending = %+v, %v", stored, err)
	}
	flaky.fail.Store(false)
	next := request(t.Context(), t, service, bot, approval.Request{ID: "next", Title: "Next"})
	if err := service.Answer(bot, "next", approval.Answer{}); err != nil {
		t.Fatal(err)
	}
	<-next
	cards := messages(t, service, records, bot)
	if len(cards) != 2 || cards[0].Approval.Request.ID != "lost" || cards[0].Approval.State != "answered" || cards[1].Approval.State != "denied" {
		t.Fatalf("cards = %+v", cards)
	}
}

func open(t *testing.T, timeout time.Duration) (*approval.Service, *store.SQLite) {
	t.Helper()
	records, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "persona.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, id := range []store.ConversationID{bot, 2} {
		commit(t, records, store.Write{Type: store.WriteConversation, Conversation: &store.Conversation{ID: id}})
	}
	return approval.NewService(records, func() time.Time { return time.UnixMilli(1_700_000_000_000) }, timeout), records
}

func commit(t *testing.T, records store.Store, batch ...store.Write) {
	t.Helper()
	if _, _, err := records.Commit(t.Context(), []store.Batch{batch}); err != nil {
		t.Fatal(err)
	}
}

type outcome struct {
	answer approval.Answer
	err    error
}

// request starts a Request and waits until it is stored.
func request(ctx context.Context, t *testing.T, service *approval.Service, botID int64, input approval.Request) <-chan outcome {
	t.Helper()
	done := make(chan outcome, 1)
	go func() {
		answer, err := service.Request(ctx, botID, input)
		done <- outcome{answer, err}
	}()
	for {
		stored, err := service.Pending(t.Context(), botID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range stored {
			// A request left by an earlier process has no RequestedAt.
			if item.ID == input.ID && item.RequestedAt != 0 {
				return done
			}
		}
		select {
		case result := <-done:
			t.Fatalf("Request returned before it was pending: %+v", result)
		case <-time.After(time.Millisecond):
		}
	}
}

// messages returns the Bot's settled approval cards and fails if any request is still stored.
func messages(t *testing.T, service *approval.Service, records *store.SQLite, botID int64) []client.Message {
	t.Helper()
	if stored, err := service.Pending(t.Context(), botID); err != nil || len(stored) != 0 {
		t.Fatalf("Pending = %+v, %v", stored, err)
	}
	page, err := records.Entries(t.Context(), store.EntryQuery{ConversationID: store.ConversationID(botID)}, store.Scan{})
	if err != nil {
		t.Fatal(err)
	}
	result := make([]client.Message, len(page.Items))
	for i, entry := range page.Items {
		if entry.Kind != approval.MessageKind {
			t.Fatalf("entry kind = %q", entry.Kind)
		}
		if err := json.Unmarshal(entry.Data, &result[i]); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestAnswerSettlesRequest(t *testing.T) {
	service, records := open(t, 0)
	done := request(t.Context(), t, service, bot, approval.Request{
		ID: "q1", Title: "Pick", Kind: approval.Question,
		Questions: []approval.QuestionItem{{ID: "color", Title: "Color?", Options: []approval.Option{{ID: "red", Label: "Red"}}}},
	})
	stored, err := service.Pending(t.Context(), bot)
	if err != nil || len(stored) != 1 || stored[0].RequestedAt != 1_700_000_000_000 || stored[0].Questions[0].Options[0].ID != "red" {
		t.Fatalf("Pending = %+v, %v", stored, err)
	}
	if err := service.Answer(2, "q1", approval.Answer{Allow: true}); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("foreign answer = %v", err)
	}
	if err := service.Answer(bot, "other", approval.Answer{Allow: true}); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("unknown answer = %v", err)
	}
	want := approval.Answer{Allow: true, Answers: map[string][]string{"color": {"red"}}}
	if err := service.Answer(bot, "q1", want); err != nil {
		t.Fatal(err)
	}
	if err := service.Answer(bot, "q1", approval.Answer{}); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("second answer = %v", err)
	}
	if result := <-done; result.err != nil || !result.answer.Allow || result.answer.Answers["color"][0] != "red" {
		t.Fatalf("Request = %+v", result)
	}
	cards := messages(t, service, records, bot)
	if len(cards) != 1 || cards[0].ID != "approval:q1:1700000000000" || cards[0].Role != "assistant" || cards[0].Pending || cards[0].Stopped {
		t.Fatalf("cards = %+v", cards)
	}
	card := cards[0].Approval
	var answer approval.Answer
	if err := json.Unmarshal(card.Answer, &answer); err != nil {
		t.Fatal(err)
	}
	if card.State != "answered" || card.Request.ID != "q1" || card.Request.Kind != approval.Question || answer.Answers["color"][0] != "red" {
		t.Fatalf("card = %+v, answer = %+v", card, answer)
	}
	if err := service.Answer(bot, "q1", want); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("stale answer = %v", err)
	}
}

func TestDenyExpireCancelAndTimeout(t *testing.T) {
	service, records := open(t, 0)
	denied := request(t.Context(), t, service, bot, approval.Request{ID: "deny", Title: "Deny"})
	if err := service.Answer(bot, "deny", approval.Answer{Text: "no"}); err != nil {
		t.Fatal(err)
	}
	if result := <-denied; result.err != nil || result.answer.Allow || result.answer.Text != "no" {
		t.Fatalf("denied = %+v", result)
	}

	withdrawn := request(t.Context(), t, service, bot, approval.Request{ID: "withdraw", Title: "Withdraw"})
	service.Expire(2, "withdraw")
	service.Expire(bot, "withdraw")
	if result := <-withdrawn; result.err != nil || result.answer.Allow {
		t.Fatalf("withdrawn = %+v", result)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancelled := request(ctx, t, service, bot, approval.Request{ID: "cancel", Title: "Cancel"})
	cancel()
	if result := <-cancelled; !errors.Is(result.err, context.Canceled) || result.answer.Allow {
		t.Fatalf("cancelled = %+v", result)
	}
	if answer, err := service.Request(ctx, bot, approval.Request{ID: "late", Title: "Late"}); !errors.Is(err, context.Canceled) || answer.Allow {
		t.Fatalf("Request after cancel = %+v, %v", answer, err)
	}

	brief, briefRecords := open(t, 20*time.Millisecond)
	if answer, err := brief.Request(t.Context(), bot, approval.Request{ID: "slow", Title: "Slow"}); err != nil || answer.Allow {
		t.Fatalf("timed out = %+v, %v", answer, err)
	}
	if cards := messages(t, brief, briefRecords, bot); len(cards) != 1 || cards[0].Approval.State != "expired" || !cards[0].Stopped {
		t.Fatalf("timed out cards = %+v", cards)
	}

	var states []string
	for _, card := range messages(t, service, records, bot) {
		states = append(states, card.Approval.Request.ID+"="+card.Approval.State)
	}
	if got := states; len(got) != 3 || got[0] != "deny=denied" || got[1] != "withdraw=expired" || got[2] != "cancel=expired" {
		t.Fatalf("states = %v", got)
	}
}

func TestRequestValidationAndDuplicates(t *testing.T) {
	service, records := open(t, 0)
	for _, bad := range []approval.Request{{Title: "No ID"}, {ID: "x"}, {ID: "x", Title: "Kind", Kind: "other"}} {
		if _, err := service.Request(t.Context(), bot, bad); err == nil {
			t.Fatalf("Request(%+v) succeeded", bad)
		}
	}
	first := request(t.Context(), t, service, bot, approval.Request{ID: "same", Title: "First"})
	if answer, err := service.Request(t.Context(), 2, approval.Request{ID: "same", Title: "Second"}); !errors.Is(err, approval.ErrDuplicate) || answer.Allow {
		t.Fatalf("duplicate = %+v, %v", answer, err)
	}
	if err := service.Answer(bot, "same", approval.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if result := <-first; result.err != nil || !result.answer.Allow {
		t.Fatalf("first = %+v", result)
	}
	if cards := messages(t, service, records, 2); len(cards) != 0 {
		t.Fatalf("other Bot cards = %+v", cards)
	}
}

func TestConcurrentRequestsKeepOtherFields(t *testing.T) {
	service, records := open(t, 0)
	// A request left by an earlier process and a field owned by other code.
	id, err := records.MintID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scope := store.Scope{Kind: "conversation", ConversationID: bot}
	commit(t, records, store.Write{
		Type:     store.WriteDocumentCreate,
		Document: &store.Document{ID: store.DocumentID(id), Kind: approval.DocKind, Scope: scope, History: "latest", Fork: "initial"},
		Value:    json.RawMessage(`{"approvals":[{"id":"r0","title":"Stale","detail":"","kind":"permission","takeover":{"id":"t"}}],"tools":[{"name":"kept"}]}`),
	})
	if err := service.Answer(bot, "r0", approval.Answer{Allow: true}); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("answer without waiter = %v", err)
	}

	ids := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	done := map[string]chan outcome{}
	for _, id := range ids {
		done[id] = make(chan outcome, 1)
		go func() {
			answer, err := service.Request(t.Context(), bot, approval.Request{ID: id, Title: id})
			done[id] <- outcome{answer, err}
		}()
	}
	for live := 0; live != len(ids); time.Sleep(time.Millisecond) {
		stored, err := service.Pending(t.Context(), bot)
		if err != nil || len(stored) > len(ids) {
			t.Fatalf("Pending = %+v, %v", stored, err)
		}
		live = 0
		for _, item := range stored {
			if item.RequestedAt != 0 {
				live++
			}
		}
	}
	var wait sync.WaitGroup
	for i, id := range ids {
		wait.Go(func() {
			if err := service.Answer(bot, id, approval.Answer{Allow: i%2 == 0}); err != nil {
				t.Error(err)
			}
			if result := <-done[id]; result.err != nil || result.answer.Allow != (i%2 == 0) {
				t.Errorf("%s = %+v", id, result)
			}
		})
	}
	wait.Wait()
	if cards := messages(t, service, records, bot); len(cards) != len(ids) {
		t.Fatalf("cards = %+v", cards)
	}
	meta, err := records.FindDocument(t.Context(), approval.DocKind, scope, "", store.Current)
	if err != nil {
		t.Fatal(err)
	}
	value, err := records.Document(t.Context(), meta.ID, store.Current)
	if err != nil || string(value.Value) != `{"approvals":[],"tools":[{"name":"kept"}]}` {
		t.Fatalf("document = %s, %v", value.Value, err)
	}
}

func TestAutomaticMode(t *testing.T) {
	service, records := open(t, 0)
	id, err := records.MintID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	commit(t, records, store.Write{
		Type:     store.WriteDocumentCreate,
		Document: &store.Document{ID: store.DocumentID(id), Kind: "persona.computers", Scope: store.Scope{Kind: "session"}},
		Value:    json.RawMessage(`{"bindings":{"1":{"approvalMode":"auto"},"2":{"approvalMode":"ask"}}}`),
	})
	if answer, err := service.Request(t.Context(), bot, approval.Request{ID: "auto", Title: "Auto", Automatic: true}); err != nil || !answer.Allow {
		t.Fatalf("automatic = %+v, %v", answer, err)
	}
	if cards := messages(t, service, records, bot); len(cards) != 0 {
		t.Fatalf("automatic cards = %+v", cards)
	}
	// "auto" mode never covers a request that is not automatic, nor a Bot in "ask" mode.
	for botID, input := range map[int64]approval.Request{
		bot: {ID: "manual", Title: "Manual"},
		2:   {ID: "ask", Title: "Ask", Automatic: true},
	} {
		done := request(t.Context(), t, service, botID, input)
		if err := service.Answer(botID, input.ID, approval.Answer{}); err != nil {
			t.Fatal(err)
		}
		if result := <-done; result.err != nil || result.answer.Allow {
			t.Fatalf("%s = %+v", input.ID, result)
		}
	}
}

type tool struct{ runs int }

func (*tool) Definition() tools.Definition {
	return tools.Definition{Name: "routine_manage", InputSchema: json.RawMessage(`{"type":"object"}`), Mutates: true}
}

func (t *tool) Run(context.Context, tools.Call) (tools.Result, error) {
	t.runs++
	return tools.Result{}, nil
}

func TestApproverGatesRegistryCalls(t *testing.T) {
	service, records := open(t, 0)
	// Even in "auto" mode a product tool call asks.
	id, err := records.MintID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	commit(t, records, store.Write{
		Type:     store.WriteDocumentCreate,
		Document: &store.Document{ID: store.DocumentID(id), Kind: "persona.computers", Scope: store.Scope{Kind: "session"}},
		Value:    json.RawMessage(`{"bindings":{"1":{"approvalMode":"auto"}}}`),
	})
	registry := tools.NewRegistry()
	mutating := &tool{}
	if err := registry.Register(mutating); err != nil {
		t.Fatal(err)
	}
	for callID, allow := range map[string]bool{"s/1": true, "s/2": false} {
		done := make(chan error, 1)
		go func() {
			_, err := registry.Call(t.Context(), tools.Call{
				BotID: bot, CallID: callID, Name: "routine_manage", Input: json.RawMessage(`{"title":"Daily","every":{"hours":24}}`),
			}, service.Approver())
			done <- err
		}()
		var stored []approval.Request
		for len(stored) == 0 {
			if stored, err = service.Pending(t.Context(), bot); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
		got := stored[0]
		if got.ID != "tool:"+callID || got.Title != "routine_manage" || got.Kind != approval.Permission || got.Automatic ||
			len(got.Fields) != 2 || got.Fields[0] != (approval.Field{Label: "every", Value: `{"hours":24}`}) || got.Fields[1] != (approval.Field{Label: "title", Value: "Daily"}) {
			t.Fatalf("stored = %+v", got)
		}
		if err := service.Answer(bot, got.ID, approval.Answer{Allow: allow}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; allow != (err == nil) || (!allow && !errors.Is(err, tools.ErrDenied)) {
			t.Fatalf("Call(%s) = %v", callID, err)
		}
	}
	if mutating.runs != 1 {
		t.Fatalf("tool ran %d times", mutating.runs)
	}
}

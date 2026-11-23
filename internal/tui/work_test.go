package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
	"github.com/Personed/Persona/internal/client/clienttest"
)

func TestWorkScreen(t *testing.T) {
	server := clienttest.Serve(t)
	m := New(t.Context(), &client.Client{BaseURL: server.URL})
	m.width, m.height = 100, 30
	m.bots = []client.Bot{{ID: 1, Name: "Ada"}}
	next, _ := m.Update(key('w', "w"))
	if got := content(next); !strings.Contains(got, "Ada · Work") || !strings.Contains(got, "loading") {
		t.Fatalf("work loading:\n%s", got)
	}
	var w client.Work
	soon := time.Now().Add(time.Hour).UnixMilli()
	if err := json.Unmarshal([]byte(`{"memory":[{"id":"m","text":"likes tea"}],"memoryReviewEnabled":true,
		"skills":[{"id":"s","name":"report","enabledBots":[1]},{"id":"o","name":"other","enabledBots":[2]}],
		"routines":{"items":[
			{"id":"r1","title":"Morning report","schedule":{"type":"cron","expression":"0 9 * * 1-5","timeZone":"UTC"},"enabled":true,"nextAt":`+itoa(int(soon))+`,"revision":3},
			{"id":"r2","title":"Backup","schedule":{"type":"cron","expression":"*/5 * * * *","timeZone":"UTC"},"enabled":false,"nextAt":null,"revision":1}],
		"runs":[{"id":"x","routineId":"r1","title":"Morning report","mode":"test","firedAt":`+itoa(int(soon))+`,"status":"failed","error":"no network"}]}}`), &w); err != nil {
		t.Fatal(err)
	}
	next, _ = next.Update(workMsg{botID: 1, work: w, stream: nil})
	got := content(next)
	for _, want := range []string{"> on   Morning report  weekdays 09:00 · next", "off  Backup  */5 * * * *", "✗ failed", "no network", "1 memories · review on · 1 of 2 skills enabled"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}

	next = press(t, next, key('e', "e"))
	if body := lastBody(t, server, "/rpc/work/routines/enable"); !strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"id":"r1"`) || !strings.Contains(body, `"revision":3`) {
		t.Fatalf("enable body %s", body)
	}
	next = press(t, next, key(tea.KeyDown, ""), key('t', "t"))
	if body := lastBody(t, server, "/rpc/work/routines/test"); !strings.Contains(body, `"id":"r2"`) || !strings.Contains(body, `"requestId"`) {
		t.Fatalf("test body %s", body)
	}
	next, _ = next.Update(key(tea.KeyEscape, ""))
	if got := content(next); !strings.Contains(got, "Persona · Bots") {
		t.Fatalf("esc did not return to Bots:\n%s", got)
	}
}

func TestScheduleLabel(t *testing.T) {
	for in, want := range map[string]string{"30 8 * * *": "daily 08:30", "0 18 * * 5": "Fridays 18:00", "0 0 1 * *": "0 0 1 * *"} {
		if got := scheduleLabel(client.Schedule{Type: "cron", Expression: in}); got != want {
			t.Errorf("scheduleLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func lastBody(t *testing.T, server *clienttest.Server, path string) string {
	t.Helper()
	requests := server.Requests()
	if len(requests) == 0 || requests[len(requests)-1].Path != path {
		t.Fatalf("last request is not %s: %+v", path, requests)
	}
	return string(requests[len(requests)-1].Body)
}

func workWith(t *testing.T, routines int, height int) tea.Model {
	t.Helper()
	m := New(t.Context(), &client.Client{BaseURL: "http://127.0.0.1:1"})
	m.width, m.height = 100, height
	m.bots = []client.Bot{{ID: 1, Name: "Ada"}}
	next, _ := m.Update(key('w', "w"))
	var w client.Work
	for i := range routines {
		w.Routines.Items = append(w.Routines.Items, client.Routine{ID: "r" + itoa(i+1), Title: "Routine " + itoa(i+1), Enabled: true})
	}
	next, _ = next.Update(workMsg{botID: 1, work: w})
	return next
}

func TestRoutineRetryReusesRequestID(t *testing.T) {
	m := press(t, workWith(t, 1, 30), key('t', "t"))
	first := m.(Model).work.pendingID
	if first == "" || m.(Model).err == nil {
		t.Fatalf("uncertain failure not kept: id %q err %v", first, m.(Model).err)
	}
	m = press(t, m, key('t', "t"))
	if got := m.(Model).work.pendingID; got != first {
		t.Fatalf("retry used a new requestId: %s then %s", first, got)
	}
	m = press(t, m, key('e', "e"))
	if got := m.(Model).work.pendingID; got == first || got == "" {
		t.Fatalf("a different action kept the old requestId %s", got)
	}
}

func TestRoutineWindowFollowsCursor(t *testing.T) {
	m := workWith(t, 20, 20)
	for range 19 {
		m = press(t, m, key(tea.KeyDown, ""))
	}
	got := content(m)
	if !strings.Contains(got, "> on   Routine 20") || strings.Contains(got, "Routine 1 ") || !strings.Contains(got, "esc back") {
		t.Fatalf("window:\n%s", got)
	}
	if lines := strings.Count(got, "\n") + 1; lines > 20 {
		t.Fatalf("Work used %d lines of 20:\n%s", lines, got)
	}
}

func TestWorkKeepsChatDraft(t *testing.T) {
	var m tea.Model = openChat(t, &client.Client{BaseURL: clienttest.Serve(t).URL})
	m = typeText(m, "half a thought")
	m = press(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, key(tea.KeyEscape, ""))
	if c := m.(Model).chat; c == nil || c.input.Value() != "half a thought" {
		t.Fatalf("draft lost after Work: %+v", c)
	}
}

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

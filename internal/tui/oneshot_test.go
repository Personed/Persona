package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/client"
)

// replyServer streams a snapshot, then the reply to the sent message in two deltas.
func replyServer(t *testing.T, final string) (*httptest.Server, *string) {
	t.Helper()
	sent := new(string)
	ready := make(chan struct{})
	var rid string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc/send":
			body, _ := io.ReadAll(r.Body)
			*sent = string(body)
			var in struct{ JSON struct{ RequestID string } }
			_ = json.Unmarshal(body, &in)
			rid = in.JSON.RequestID
			_, _ = io.WriteString(w, `{"json":{"ok":true}}`)
			close(ready)
		case "/rpc/changes":
			w.Header().Set("Content-Type", "text/event-stream")
			event := func(v string) {
				fmt.Fprintf(w, "event: message\ndata: {\"json\":%s}\n\n", v)
				w.(http.Flusher).Flush()
			}
			old := `{"id":"u0","role":"user","content":[{"type":"text","text":"earlier"}]},{"id":"a0","role":"assistant","content":[{"type":"text","text":"old reply"}]}`
			event(`{"type":"snapshot","view":{"messages":[` + old + `],"busy":false}}`)
			<-ready
			user := `{"id":"u1","role":"user","turnId":"t1","requestId":"` + rid + `","content":[{"type":"text","text":"hi"}]}`
			event(`{"type":"update","messages":[{"id":"a9","role":"assistant","turnId":"t0","content":[{"type":"text","text":"other turn"}]},` + user + `],"state":{"busy":true,"activeTurnId":"t0"}}`)
			event(`{"type":"update","messages":[],"state":{"activeTurnId":"t1","draft":{"id":"d","role":"assistant","turnId":"t1","content":[{"type":"text","text":"Hel"}]}}}`)
			event(`{"type":"update","messages":[{"id":"a1","role":"assistant","turnId":"t1","content":[{"type":"text","text":"Hello!"}]}],"state":` + final + `}`)
			<-r.Context().Done()
		}
	}))
	t.Cleanup(server.Close)
	return server, sent
}

func TestChatOnce(t *testing.T) {
	server, sent := replyServer(t, `{"busy":false,"draft":null,"activeTurnId":null}`)
	var out bytes.Buffer
	err := chatOnce(t.Context(), &client.Client{BaseURL: server.URL}, client.Bot{ID: 3, Name: "Ada"}, "hi", &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Ada> Hello!\n" {
		t.Fatalf("output %q", out.String())
	}
	if !strings.Contains(*sent, `"botId":3`) || !strings.Contains(*sent, `"text":"hi"`) || !strings.Contains(*sent, `"requestId":"`) {
		t.Fatalf("send body %s", *sent)
	}
}

func TestChatOnceFailure(t *testing.T) {
	server, _ := replyServer(t, `{"busy":false,"draft":null,"activeTurnId":null,"failed":true,"error":"quota exceeded"}`)
	var out bytes.Buffer
	err := chatOnce(t.Context(), &client.Client{BaseURL: server.URL}, client.Bot{ID: 3, Name: "Ada"}, "hi", &out)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v, output %q", err, out.String())
	}
}

func TestPickBot(t *testing.T) {
	bots := []client.Bot{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}}
	if b, _ := pickBot(bots, 0); b.ID != 1 {
		t.Fatalf("default bot %d", b.ID)
	}
	if b, _ := pickBot(bots, 2); b.Name != "B" {
		t.Fatalf("bot 2 = %+v", b)
	}
	if _, err := pickBot(bots, 9); err == nil {
		t.Fatal("missing bot accepted")
	}
	if _, err := pickBot(nil, 0); err == nil {
		t.Fatal("empty list accepted")
	}
}

func TestHistoryPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"before":"p1"`) {
			_, _ = io.WriteString(w, `{"json":{"messages":[{"id":"1","role":"user","content":[{"type":"text","text":"first"}]}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"json":{"before":"p1","messages":[{"id":"2","role":"assistant","content":[{"type":"text","text":"second"}]}]}}`)
	}))
	t.Cleanup(server.Close)
	msgs, err := history(t.Context(), &client.Client{BaseURL: server.URL}, 1)
	if err != nil || len(msgs) != 2 || msgs[0].Text() != "first" || msgs[1].Text() != "second" {
		t.Fatalf("history = %+v, %v", msgs, err)
	}
}

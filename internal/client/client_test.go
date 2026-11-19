package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/client/clienttest"
)

var fixtures = filepath.Join("..", "..", "contracts", "api")

func TestCallFixtures(t *testing.T) {
	server := clienttest.New(t, fixtures)
	c := &Client{BaseURL: server.URL}
	ctx := context.Background()

	var bots []Bot
	if err := c.Call(ctx, "bots.list", nil, &bots); err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].ID != 1 || bots[0].Name != "Persona" {
		t.Fatalf("bots.list = %+v", bots)
	}

	var view ChatView
	if err := c.Call(ctx, "get", map[string]int{"botId": 1}, &view); err != nil {
		t.Fatal(err)
	}
	if view.ID != 1 || view.Model == nil || view.Model.ModelID != "faux-1" {
		t.Fatalf("get = %+v", view)
	}

	var sent struct {
		ID int `json:"id"`
	}
	if err := c.Call(ctx, "send", map[string]any{"botId": 1, "text": "hello", "requestId": "r1"}, &sent); err != nil || sent.ID != 11 {
		t.Fatalf("send = %+v, %v", sent, err)
	}
	requests := server.Requests()
	if got := string(requests[len(requests)-1].Body); got != `{"json":{"botId":1,"requestId":"r1","text":"hello"}}` {
		t.Fatalf("send body %s", got)
	}
	if requests[0].Path != "/rpc/bots/list" || string(requests[0].Body) != "{}" {
		t.Fatalf("bots.list request %+v", requests[0])
	}

	if err := c.Call(ctx, "get", map[string]int{"botId": 1}, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) == 0 || view.Messages[0].Role != "user" || view.Messages[0].Text() != "hello" {
		t.Fatalf("get after send = %+v", view.Messages)
	}
}

func TestCallErrors(t *testing.T) {
	server := clienttest.New(t, fixtures)
	c := &Client{BaseURL: server.URL}
	err := c.Call(context.Background(), "engines.get", map[string]int{"botId": 999999}, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Status != 404 || Code(err) != "NOT_FOUND" {
		t.Fatalf("engines.get error = %v", err)
	}
	err = c.Call(context.Background(), "computer.list", nil, nil)
	if Code(err) != "SERVICE_UNAVAILABLE" || !errors.As(err, &rpcErr) || rpcErr.Message == "" {
		t.Fatalf("computer.list error = %v", err)
	}
}

func TestSubscribeFixture(t *testing.T) {
	server := clienttest.New(t, fixtures)
	c := &Client{BaseURL: server.URL}
	stream, err := c.Subscribe(context.Background(), "changes", map[string]int{"botId": 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var change Change
	if err := stream.Next(&change); err != nil {
		t.Fatal(err)
	}
	var view ChatView
	if err := view.Apply(change); err != nil {
		t.Fatal(err)
	}
	if change.Type != "snapshot" || view.ID != 1 || len(view.Messages) == 0 {
		t.Fatalf("snapshot = %+v", change)
	}
	if err := stream.Next(&change); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v", err)
	}
}

func TestStreamEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Accept") != "text/event-stream" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": ping\n\nevent: message\ndata: {\"json\":\ndata: 1}\n\nevent: message\ndata: {\"json\":2}\n\nevent: error\ndata: {\"json\":{\"defined\":false,\"code\":\"CONFLICT\",\"status\":409,\"message\":\"busy\"}}\n\n")
	}))
	defer server.Close()
	stream, err := (&Client{BaseURL: server.URL, Token: "token"}).Subscribe(context.Background(), "subscribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, want := range []int{1, 2} {
		var got int
		if err := stream.Next(&got); err != nil || got != want {
			t.Fatalf("Next = %d, %v; want %d", got, err, want)
		}
	}
	if err := stream.Next(nil); Code(err) != "CONFLICT" {
		t.Fatalf("error event = %v", err)
	}
}

func TestApplyUpdate(t *testing.T) {
	view := ChatView{ID: 1, Messages: []Message{{ID: "a", Role: "user"}, {ID: "b", Role: "assistant"}, {ID: "c", Role: "assistant"}}}
	err := view.Apply(Change{
		Type:     "update",
		State:    map[string]json.RawMessage{"busy": json.RawMessage("true"), "activeTurnId": json.RawMessage(`"t"`)},
		Messages: []Message{{ID: "b", Role: "assistant", Content: []Content{{Type: "text", Text: "hi"}}}, {ID: "d", Role: "user"}},
		Removed:  []string{"c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range view.Messages {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "a,b,d" || view.Messages[1].Text() != "hi" || !view.Busy || view.ActiveTurnID == nil || *view.ActiveTurnID != "t" {
		t.Fatalf("view = %+v", view)
	}
}

func TestPairing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hosting/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"product":"persona","version":"1"}`)
	})
	mux.HandleFunc("POST /hosting/exchange", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code, Label string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Code != "ABCD-1234" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid code"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "secret", HttpOnly: true})
		_, _ = io.WriteString(w, `{"device":{"id":"d1","label":"`+in.Label+`","scope":"owner","created":1767225600000,"expires":1769904000000}}`)
	})
	mux.HandleFunc("GET /hosting/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"device":{"id":"d1","label":"tui","scope":"owner"}}`)
	})
	mux.HandleFunc("POST /hosting/logout", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx := context.Background()
	c := &Client{BaseURL: server.URL + "/"}
	if _, err := c.Info(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pair(ctx, "wrong", "tui"); err == nil || !strings.Contains(err.Error(), "invalid code") {
		t.Fatalf("Pair(wrong) = %v", err)
	}
	device, err := c.Pair(ctx, "ABCD-1234", "tui")
	if err != nil || device.ID != "d1" || device.Label != "tui" || c.Token != "secret" {
		t.Fatalf("Pair = %+v, %v, token %q", device, err, c.Token)
	}
	if device, err := c.Session(ctx); err != nil || device.ID != "d1" {
		t.Fatalf("Session = %+v, %v", device, err)
	}
	if err := c.Logout(ctx); err != nil || c.Token != "" {
		t.Fatalf("Logout = %v, token %q", err, c.Token)
	}
	if _, err := c.Session(ctx); Code(err) != "Unauthorized" {
		t.Fatalf("Session after logout = %v", err)
	}
}

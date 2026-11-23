package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(t *testing.T, h http.Handler, path, input string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rpc/"+strings.ReplaceAll(path, ".", "/"), strings.NewReader(`{"json":`+input+`}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func rootBot(t *testing.T, h http.Handler) string {
	t.Helper()
	_, body := call(t, h, "bots.list", "{}")
	var out struct {
		JSON []struct {
			ID json.Number `json:"id"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.JSON) == 0 {
		t.Fatalf("bots.list: %s", body)
	}
	return out.JSON[0].ID.String()
}

func TestModelProcedures(t *testing.T) {
	h, _ := fixtureService(t, "models-work")
	bot := rootBot(t, h)
	for _, c := range []struct {
		path, input string
		status      int
		body        string
	}{
		{"models.thinking", `{"botId":` + bot + `,"thinkingLevel":"high"}`, 400, `"BAD_REQUEST"`},
		{"models.thinking", `{"botId":` + bot + `,"thinkingLevel":"off"}`, 200, `{"json":{"ok":true}}`},
		{"models.thinking", `{"botId":999,"thinkingLevel":"off"}`, 404, `"NOT_FOUND"`},
		{"models.select", `{"botId":` + bot + `,"provider":"faux","modelId":"faux-1"}`, 200, `{"json":{"provider":"faux","modelId":"faux-1"}}`},
		{"models.select", `{"botId":` + bot + `,"provider":"faux","modelId":"missing"}`, 412, `"PRECONDITION_FAILED"`},
		{"models.select", `{"botId":999,"provider":"faux","modelId":"faux-1"}`, 404, `"NOT_FOUND"`},
		{"models.refresh", `{"provider":"faux"}`, 200, `"modelId":"faux-1"`},
		{"models.refresh", `{"provider":"other"}`, 400, `"BAD_REQUEST"`},
		{"engines.usage", `{}`, 200, `{"json":[]}`},
		{"engines.compact", `{"botId":999,"requestId":"00000000-0000-4000-8000-000000000001"}`, 404, `"NOT_FOUND"`},
		{"engines.controls", `{"botId":` + bot + `}`, 501, `"NOT_IMPLEMENTED"`},
	} {
		status, body := call(t, h, c.path, c.input)
		if status != c.status || !strings.Contains(body, c.body) {
			t.Errorf("%s %s = %d %s, want %d %s", c.path, c.input, status, body, c.status, c.body)
		}
	}
}

func TestNativeModelSelection(t *testing.T) {
	h, _ := fixtureService(t, "native-conversation")
	bot := rootBot(t, h)
	if status, _ := call(t, h, "models.select", `{"botId":`+bot+`,"provider":"pi","modelId":"test-model"}`); status != http.StatusNotImplemented {
		t.Errorf("models.select = %d, want 501 until native controls are composed", status)
	}
	if status, body := call(t, h, "send", `{"botId":`+bot+`,"text":"hello","requestId":"00000000-0000-4000-8000-000000000002"}`); status != http.StatusOK {
		t.Fatalf("send = %d %s", status, body)
	}
	if status, body := call(t, h, "engines.usage", `{"botId":`+bot+`,"harness":"pi"}`); status != http.StatusOK || !strings.Contains(body, `"provider":"pi"`) {
		t.Errorf("engines.usage = %d %s", status, body)
	}
}

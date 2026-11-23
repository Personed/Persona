package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/contract"
)

const zeroUUID = "00000000-0000-0000-0000-000000000000"

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(loadContract(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, s http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// TestFixtures replays every recorded exchange. Handlers answer with the recorded
// output or error, so the test checks decoding, validation, and the envelopes.
func TestFixtures(t *testing.T) {
	fixtures, err := contract.Fixtures(filepath.Join("..", "..", "contracts", "api"))
	if err != nil {
		t.Fatal(err)
	}
	for name, exchanges := range fixtures {
		for i, ex := range exchanges {
			s := newServer(t)
			var recorded struct {
				JSON json.RawMessage `json:"json"`
			}
			if ex.Response.ContentType == "" {
				if err := json.Unmarshal(ex.Response.Body, &recorded); err != nil {
					t.Fatalf("%s %d: %v", name, i, err)
				}
			}
			answer := func() (any, error) {
				if ex.Response.Status == http.StatusOK {
					return recorded.JSON, nil
				}
				var e struct {
					Code    string          `json:"code"`
					Message string          `json:"message"`
					Data    json.RawMessage `json:"data"`
				}
				_ = json.Unmarshal(recorded.JSON, &e)
				if e.Message == "Input validation failed" {
					t.Errorf("%s %d: handler called for invalid input", name, i)
				}
				return nil, NewError(e.Code, e.Message)
			}
			p, _ := s.Procedure(ex.Request.Path)
			if p.Kind == "stream" {
				var frame string
				_ = json.Unmarshal(ex.Response.Body, &frame)
				s.HandleStream(p.Path, func(_ context.Context, _ json.RawMessage, send func(any) error) (any, error) {
					data := strings.TrimPrefix(strings.SplitN(frame, "\n", 3)[1], "data: ")
					var value struct {
						JSON json.RawMessage `json:"json"`
					}
					_ = json.Unmarshal([]byte(data), &value)
					return Void, send(value.JSON)
				})
			} else {
				s.Handle(p.Path, func(context.Context, json.RawMessage) (any, error) { return answer() })
			}
			body := strings.ReplaceAll(string(ex.Request.Body), "<uuid>", zeroUUID)
			w := post(t, s, ex.Request.Path, body)
			if w.Code != ex.Response.Status {
				t.Errorf("%s %d %s: status %d, want %d", name, i, ex.Procedure, w.Code, ex.Response.Status)
				continue
			}
			if p.Kind == "stream" {
				var frame string
				_ = json.Unmarshal(ex.Response.Body, &frame)
				if got := w.Body.String(); !strings.HasPrefix(got, ": \n\n"+frame) {
					t.Errorf("%s %d %s: stream %q, want first frame %q", name, i, ex.Procedure, got, frame)
				}
				continue
			}
			if got, want := canonical(t, w.Body.Bytes()), canonical(t, ex.Response.Body); !bytes.Equal(got, want) {
				t.Errorf("%s %d %s:\n got %s\nwant %s", name, i, ex.Procedure, got, want)
			}
		}
	}
}

func TestErrors(t *testing.T) {
	s := newServer(t)
	s.Handle("bots.list", func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("secret detail") })
	w := post(t, s, "/rpc/bots/list", "{}")
	want := `{"json":{"defined":false,"code":"INTERNAL_SERVER_ERROR","status":500,"message":"Internal server error"}}`
	if w.Code != 500 || w.Body.String() != want {
		t.Errorf("unexpected error: %d %s", w.Code, w.Body)
	}
	if w := post(t, s, "/rpc/models/list", "{}"); w.Code != 501 {
		t.Errorf("unregistered procedure: %d %s", w.Code, w.Body)
	}
	if w := post(t, s, "/rpc/nope", "{}"); w.Code != 404 {
		t.Errorf("unknown path: %d", w.Code)
	}
	s.Handle("get", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	if w := post(t, s, "/rpc/get", "{"); w.Code != 400 {
		t.Errorf("malformed body: %d", w.Code)
	}
}

func TestInputAndOutput(t *testing.T) {
	s := newServer(t)
	var got json.RawMessage
	s.Handle("bots.create", func(_ context.Context, input json.RawMessage) (any, error) {
		got = input
		return Void, nil
	})
	w := post(t, s, "/rpc/bots/create", `{"json":{"name":"  Ada \u00a0","requestId":"`+zeroUUID+`","extra":1}}`)
	if w.Code != 200 || w.Body.String() != "{}" {
		t.Fatalf("void output: %d %s", w.Code, w.Body)
	}
	if want := `{"name":"Ada","instructions":"","requestId":"` + zeroUUID + `"}`; string(got) != want {
		t.Errorf("input %s, want %s", got, want)
	}
	r := httptest.NewRequest(http.MethodGet, `/rpc/bots/create?data={"json":{"name":"B","requestId":"`+zeroUUID+`"}}`, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(string(got), `"name":"B"`) {
		t.Errorf("GET input: %d %s", rec.Code, got)
	}
}

func TestMultipart(t *testing.T) {
	s := newServer(t)
	var got struct {
		File Blob `json:"file"`
	}
	s.Handle("files.upload", func(_ context.Context, input json.RawMessage) (any, error) {
		return nil, json.Unmarshal(input, &got)
	})
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("data", `{"json":{"botId":1,"file":{}},"maps":[["file"]]}`)
	part, _ := form.CreateFormFile("0", "a.txt")
	_, _ = part.Write([]byte("hello"))
	_ = form.Close()
	r := httptest.NewRequest(http.MethodPost, "/rpc/files/upload", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || got.File.Name != "a.txt" || string(got.File.Data) != "hello" {
		t.Errorf("upload: %d %s %+v", w.Code, w.Body, got.File)
	}
}

func TestStream(t *testing.T) {
	s := newServer(t)
	s.keepAlive = 10 * time.Millisecond
	s.HandleStream("subscribe", func(_ context.Context, _ json.RawMessage, send func(any) error) (any, error) {
		if err := send(map[string]int{"n": 1}); err != nil {
			return nil, err
		}
		time.Sleep(30 * time.Millisecond)
		return nil, NewError("NOT_FOUND", "")
	})
	s.HandleStream("work.subscribe", func(context.Context, json.RawMessage, func(any) error) (any, error) {
		return "end", nil
	})
	w := post(t, s, "/rpc/subscribe", `{"json":{"botId":1}}`)
	body := w.Body.String()
	if w.Header().Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(body, ": \n\nevent: message\ndata: {\"json\":{\"n\":1}}\n\n: \n\n") ||
		!strings.HasSuffix(body, "event: error\ndata: {\"json\":{\"defined\":false,\"code\":\"NOT_FOUND\",\"status\":404,\"message\":\"Not Found\"}}\n\n") {
		t.Errorf("stream body %q", body)
	}
	w = post(t, s, "/rpc/work/subscribe", `{"json":{"botId":1}}`)
	if body := w.Body.String(); body != ": \n\nevent: done\ndata: {\"json\":\"end\"}\n\n" {
		t.Errorf("done body %q", body)
	}
}

func TestStreamCancel(t *testing.T) {
	s := newServer(t)
	stopped := make(chan struct{}, 1)
	s.HandleStream("subscribe", func(ctx context.Context, _ json.RawMessage, send func(any) error) (any, error) {
		for {
			if err := send(1); err != nil {
				stopped <- struct{}{}
				return nil, err
			}
			select {
			case <-ctx.Done():
				stopped <- struct{}{}
				return nil, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	})
	server := httptest.NewServer(s)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/rpc/subscribe", strings.NewReader(`{"json":{"botId":1}}`))
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadFull(resp.Body, make([]byte, 20))
	cancel()
	_ = resp.Body.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop after the client left")
	}
}

func TestCloseStreamsAbortsConnection(t *testing.T) {
	s := newServer(t)
	s.HandleStream("subscribe", func(ctx context.Context, _ json.RawMessage, send func(any) error) (any, error) {
		if err := send(1); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	server := httptest.NewServer(s)
	defer server.Close()
	resp, err := http.Post(server.URL+"/rpc/subscribe", "application/json", strings.NewReader(`{"json":{"botId":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadFull(resp.Body, make([]byte, 10))
	s.CloseStreams()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("closed stream ended cleanly, want an aborted connection")
	}
}

func TestVoidStreamDone(t *testing.T) {
	s := newServer(t)
	s.HandleStream("work.subscribe", func(context.Context, json.RawMessage, func(any) error) (any, error) {
		return Void, nil
	})
	w := post(t, s, "/rpc/work/subscribe", `{"json":{"botId":1}}`)
	if body := w.Body.String(); body != ": \n\nevent: done\ndata: {}\n\n" {
		t.Errorf("void done body %q", body)
	}
}

func TestInfiniteNumber(t *testing.T) {
	s, _ := CompileSchema(json.RawMessage(`{"type":"object","properties":{"from":{"type":"number","minimum":0}}}`))
	input, _ := decode([]byte(`{"from":1e309}`))
	_, issues := s.Validate(input)
	got, _ := marshal(issues)
	if want := `[{"expected":"number","code":"invalid_type","received":"Infinity","path":["from"],"message":"Invalid input: expected number, received Infinity"}]`; string(got) != want {
		t.Errorf("issues %s", got)
	}
}

func TestBlobOutput(t *testing.T) {
	w := httptest.NewRecorder()
	writeOutput(w, map[string]any{"file": &Blob{Name: `a"b.txt`, Type: "text/plain", Data: []byte("ola")}, "n": 1})
	_, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || w.Code != 200 {
		t.Fatalf("response %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	form, err := multipart.NewReader(w.Body, params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if data := form.Value["data"]; len(data) != 1 || data[0] != `{"json":{"file":{},"n":1},"maps":[["file"]]}` {
		t.Errorf("data %q", data)
	}
	file := form.File["0"]
	if len(file) != 1 || file[0].Filename != "a%22b.txt" || file[0].Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("file part %+v", file)
	}
	f, _ := file[0].Open()
	if content, _ := io.ReadAll(f); string(content) != "ola" {
		t.Errorf("content %q", content)
	}
}

func TestOutputEdgeCases(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["a"], cyclic["b"] = cyclic, cyclic
	shared := []any{1}
	var nilSlice []any
	var nilBlob *Blob
	for _, c := range []struct {
		output any
		status int
		body   string
	}{
		{cyclic, 500, `{"json":{"defined":false,"code":"INTERNAL_SERVER_ERROR","status":500,"message":"Internal server error"}}`},
		{map[string]any{"file": nilBlob, "list": nilSlice}, 200, `{"json":{"file":null,"list":null}}`},
		{map[string]any{"x": shared, "y": shared}, 200, `{"json":{"x":[1],"y":[1]}}`},
	} {
		w := httptest.NewRecorder()
		writeOutput(w, c.output)
		if w.Code != c.status || w.Body.String() != c.body {
			t.Errorf("writeOutput: %d %s", w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	writeOutput(w, map[string]any{"file": &Blob{Data: []byte("x")}, "list": nilSlice, "none": nilBlob})
	_, params, _ := mime.ParseMediaType(w.Header().Get("Content-Type"))
	form, err := multipart.NewReader(w.Body, params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if data := form.Value["data"]; len(data) != 1 || data[0] != `{"json":{"file":{},"list":null,"none":null},"maps":[["file"]]}` {
		t.Errorf("data %q", data)
	}
	if file := form.File["0"]; len(file) != 1 || file[0].Filename != "blob" || file[0].Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("file part %+v", file)
	}
}

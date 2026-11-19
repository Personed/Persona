package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/contract"
)

// Unary answers a unary procedure. Input is the validated input JSON, or nil when the
// request had none. Returning Void answers with no output.
type Unary func(ctx context.Context, input json.RawMessage) (any, error)

// Stream answers a stream procedure: send writes one message event. Its result,
// unless Void, is the value of the done event, and its error the error event. Like the
// TypeScript async generators, a stream reports every handler error as an event.
type Stream func(ctx context.Context, input json.RawMessage, send func(value any) error) (any, error)

// Void is the JavaScript undefined result: no "json" field, or no done data.
var Void any = undefined

// KeepAlive is the interval of SSE keep-alive comments, as the oRPC default.
const KeepAlive = 5 * time.Second

type procedure struct {
	contract.Procedure
	schema *Schema
	unary  Unary
	stream Stream
}

// Server serves the procedures of one contract.
type Server struct {
	prefix     string
	procedures map[string]*procedure
	keepAlive  time.Duration
}

// New compiles every input schema of c.
func New(c contract.Contract) (*Server, error) {
	s := &Server{prefix: c.Prefix, procedures: map[string]*procedure{}, keepAlive: KeepAlive}
	for _, p := range c.Procedures {
		proc := &procedure{Procedure: p}
		if len(p.Input) > 0 && string(p.Input) != "null" {
			schema, err := CompileSchema(p.Input)
			if err != nil {
				return nil, fmt.Errorf("%s input: %w", p.Path, err)
			}
			proc.schema = schema
		}
		s.procedures[p.HTTPPath(c.Prefix)] = proc
	}
	return s, nil
}

// Handle registers a unary procedure. It panics if path is not a unary contract procedure.
func (s *Server) Handle(path string, h Unary) {
	s.lookup(path, "unary").unary = h
}

// HandleStream registers a stream procedure. It panics if path is not a stream contract procedure.
func (s *Server) HandleStream(path string, h Stream) {
	s.lookup(path, "stream").stream = h
}

func (s *Server) lookup(path, kind string) *procedure {
	p, ok := s.procedures[contract.Procedure{Path: path}.HTTPPath(s.prefix)]
	if !ok || p.Kind != kind {
		panic(fmt.Sprintf("rpc: %s is not a %s procedure of the contract", path, kind))
	}
	return p
}

// Procedure returns the contract procedure served at the request path.
func (s *Server) Procedure(urlPath string) (contract.Procedure, bool) {
	p, ok := s.procedures[urlPath]
	if !ok {
		return contract.Procedure{}, false
	}
	return p.Procedure, true
}

// ServeHTTP answers one oRPC request. Paths outside the contract are 404 without an
// oRPC body, so callers can fall through to other routes with Procedure first.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := s.procedures[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	raw, err := readInput(r)
	if err != nil {
		writeError(w, NewError("BAD_REQUEST", "Cannot parse request body"))
		return
	}
	input := raw
	if p.schema != nil {
		parsed, issues := p.schema.Validate(raw)
		if len(issues) > 0 {
			writeError(w, NewError("BAD_REQUEST", "Input validation failed").WithData(map[string]any{"issues": issues}))
			return
		}
		input = parsed
	}
	// Validation comes first: in the TypeScript router every procedure exists.
	if p.unary == nil && p.stream == nil {
		writeError(w, NewError("NOT_IMPLEMENTED", ""))
		return
	}
	var inputJSON json.RawMessage
	if _, missing := input.(undefinedValue); !missing {
		if inputJSON, err = marshal(input); err != nil {
			writeError(w, toError(err))
			return
		}
	}
	if p.unary != nil {
		output, err := p.unary(r.Context(), inputJSON)
		if err != nil {
			writeError(w, toError(err))
			return
		}
		writeOutput(w, output)
		return
	}
	s.serveEvents(w, r, p.stream, inputJSON)
}

func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request, stream Stream, input json.RawMessage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	writes := make(chan string)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(s.keepAlive)
		defer ticker.Stop()
		flusher, _ := w.(http.Flusher)
		write := func(text string) {
			_, _ = io.WriteString(w, text)
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(": \n\n")
		for {
			select {
			case text, ok := <-writes:
				if !ok {
					return
				}
				write(text)
				ticker.Reset(s.keepAlive)
			case <-ticker.C:
				write(": \n\n")
			}
		}
	}()
	emit := func(event string, value any) error {
		text := "event: " + event + "\n"
		if _, missing := value.(undefinedValue); !missing {
			data, err := marshal(value)
			if err != nil {
				return err
			}
			text += "data: " + string(data) + "\n"
		}
		select {
		case writes <- text + "\n":
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}
	result, err := stream(r.Context(), input, func(value any) error { return emit("message", envelope(value)) })
	switch {
	case r.Context().Err() != nil:
	case err != nil:
		_ = emit("error", map[string]any{"json": toError(err)})
	default:
		_ = emit("done", envelope(result))
	}
	close(writes)
	<-finished
}

// envelope is the RPC serializer body of value: {"json": value}, or {} for Void.
func envelope(value any) any {
	if _, missing := value.(undefinedValue); missing {
		return struct{}{}
	}
	return map[string]any{"json": value}
}

// writeOutput writes a unary result. Like the oRPC serializer, it sends an
// output holding Blobs as multipart form data: field "data" is the JSON body
// with each Blob's path in "maps", and field i holds the i-th Blob. Blobs are
// found in maps, Objects and slices, not in struct fields.
func writeOutput(w http.ResponseWriter, output any) {
	walk := blobWalk{open: map[uintptr]bool{}}
	value, ok := walk.value(output)
	if !ok {
		writeError(w, toError(errors.New("rpc: output contains a cycle")))
		return
	}
	maps, blobs := walk.maps, walk.blobs
	if len(blobs) == 0 {
		writeJSON(w, http.StatusOK, envelope(output))
		return
	}
	data, err := marshal(map[string]any{"json": value, "maps": maps})
	if err != nil {
		writeError(w, toError(err))
		return
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("data", string(data))
	escape := strings.NewReplacer("\n", "%0A", "\r", "%0D", `"`, "%22")
	for i, blob := range blobs {
		name, kind := blob.Name, blob.Type
		if name == "" {
			name = "blob"
		}
		if kind == "" {
			kind = "application/octet-stream"
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%d"; filename="%s"`, i, escape.Replace(name)))
		header.Set("Content-Type", kind)
		part, _ := form.CreatePart(header)
		_, _ = part.Write(blob.Data)
	}
	_ = form.Close()
	w.Header().Set("Content-Type", form.FormDataContentType())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

// blobWalk copies an output with each Blob replaced by {}, as JSON.stringify
// writes a Blob, and records the Blobs and their paths. It stops at the first
// map, Object or slice that contains itself.
type blobWalk struct {
	maps  [][]any
	blobs []*Blob
	path  []any
	open  map[uintptr]bool
}

func (b *blobWalk) value(v any) (any, bool) {
	switch t := v.(type) {
	case Blob:
		return b.value(&t)
	case *Blob:
		if t == nil {
			return nil, true
		}
		b.maps = append(b.maps, append([]any{}, b.path...))
		b.blobs = append(b.blobs, t)
		return struct{}{}, true
	case map[string]any:
		if t == nil {
			return v, true
		}
		out := make(map[string]any, len(t))
		return out, b.within(reflect.ValueOf(t).Pointer(), func() bool {
			for key, item := range t {
				var ok bool
				if out[key], ok = b.child(key, item); !ok {
					return false
				}
			}
			return true
		})
	case *Object:
		if t == nil {
			return v, true
		}
		out := &Object{Keys: t.Keys, Values: make(map[string]any, len(t.Values))}
		return out, b.within(reflect.ValueOf(t).Pointer(), func() bool {
			for _, key := range t.Keys {
				var ok bool
				if out.Values[key], ok = b.child(key, t.Values[key]); !ok {
					return false
				}
			}
			return true
		})
	case []any:
		if t == nil {
			return v, true
		}
		out := make([]any, len(t))
		if len(t) == 0 {
			return out, true
		}
		return out, b.within(reflect.ValueOf(t).Pointer(), func() bool {
			for i, item := range t {
				var ok bool
				if out[i], ok = b.child(i, item); !ok {
					return false
				}
			}
			return true
		})
	}
	return v, true
}

func (b *blobWalk) within(container uintptr, walk func() bool) bool {
	if b.open[container] {
		return false
	}
	b.open[container] = true
	defer delete(b.open, container)
	return walk()
}

func (b *blobWalk) child(key, v any) (any, bool) {
	b.path = append(b.path, key)
	defer func() { b.path = b.path[:len(b.path)-1] }()
	return b.value(v)
}

func writeError(w http.ResponseWriter, err *Error) {
	writeJSON(w, err.Status, map[string]any{"json": err})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		data, _ = marshal(map[string]any{"json": NewError("INTERNAL_SERVER_ERROR", "Internal server error")})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// readInput decodes an RPC serializer body: JSON {"json": input}, a GET ?data= query,
// or multipart form data whose "data" field is the JSON body and whose numbered
// fields are the files listed in "maps".
func readInput(r *http.Request) (any, error) {
	if r.Method == http.MethodGet {
		data := r.URL.Query().Get("data")
		if data == "" {
			return undefined, nil
		}
		return unwrap([]byte(data))
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		return readMultipart(r)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return undefined, nil
	}
	return unwrap(body)
}

func unwrap(body []byte) (any, error) {
	value, err := decode(body)
	if err != nil {
		return nil, err
	}
	object, ok := value.(*Object)
	if !ok {
		return nil, fmt.Errorf("body is not an object")
	}
	input, ok := object.Get("json")
	if !ok {
		return undefined, nil
	}
	return input, nil
}

func readMultipart(r *http.Request) (any, error) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, err
	}
	data := r.MultipartForm.Value["data"]
	if len(data) != 1 {
		return nil, fmt.Errorf("multipart body has no data field")
	}
	value, err := decode([]byte(data[0]))
	if err != nil {
		return nil, err
	}
	object, ok := value.(*Object)
	if !ok {
		return nil, fmt.Errorf("data is not an object")
	}
	input, ok := object.Get("json")
	if !ok {
		input = undefined
	}
	maps, _ := object.Get("maps")
	paths, _ := maps.([]any)
	for i, path := range paths {
		files := r.MultipartForm.File[strconv.Itoa(i)]
		if len(files) != 1 {
			return nil, fmt.Errorf("multipart body has no file %d", i)
		}
		file, err := files[0].Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		blob := &Blob{Name: files[0].Filename, Type: files[0].Header.Get("Content-Type"), Data: content}
		segments, _ := path.([]any)
		if input, err = setPath(input, segments, blob); err != nil {
			return nil, err
		}
	}
	return input, nil
}

func setPath(root any, path []any, value any) (any, error) {
	if len(path) == 0 {
		return value, nil
	}
	switch container := root.(type) {
	case *Object:
		key := fmt.Sprint(path[0])
		child, _ := container.Get(key)
		updated, err := setPath(child, path[1:], value)
		if err != nil {
			return nil, err
		}
		container.Set(key, updated)
		return container, nil
	case []any:
		index, err := strconv.Atoi(fmt.Sprint(path[0]))
		if err != nil || index < 0 || index >= len(container) {
			return nil, fmt.Errorf("invalid file path %v", path)
		}
		container[index], err = setPath(container[index], path[1:], value)
		return container, err
	}
	return nil, fmt.Errorf("invalid file path %v", path)
}

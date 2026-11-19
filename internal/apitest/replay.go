// Package apitest replays the recorded contracts/api fixtures against a Go
// handler, normalizing values the way apps/api/test/contract.test.ts records them.
package apitest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/contract"
)

// RequestUUID replaces the <uuid> placeholders in recorded requests.
const RequestUUID = "00000000-0000-4000-8000-000000000000"

// Result is one replayed exchange. Bodies are normalized canonical JSON; a
// stream body is its first event frame.
type Result struct {
	File       string
	Index      int
	Procedure  string
	Status     int
	WantStatus int
	Body       string
	WantBody   string
}

// Match reports whether the handler answered as recorded.
func (r Result) Match() bool { return r.Status == r.WantStatus && r.Body == r.WantBody }

func (r Result) String() string {
	return fmt.Sprintf("%s #%d %s: got %d %s, want %d %s", r.File, r.Index, r.Procedure, r.Status, r.Body, r.WantStatus, r.WantBody)
}

// Replay sends every fixture exchange in dir to h, one file at a time in
// recorded order, so a stateful handler sees each recorded session in sequence.
// newHandler is called once per fixture file.
func Replay(dir string, newHandler func(file string) http.Handler) ([]Result, error) {
	fixtures, err := contract.Fixtures(dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(fixtures))
	for name := range fixtures {
		files = append(files, name)
	}
	slices.Sort(files)
	var results []Result
	for _, file := range files {
		server := httptest.NewServer(newHandler(file))
		for i, ex := range fixtures[file] {
			result, err := replay(server.URL, ex)
			if err != nil {
				server.Close()
				return nil, fmt.Errorf("%s #%d %s: %w", file, i, ex.Procedure, err)
			}
			result.File, result.Index = file, i
			results = append(results, result)
		}
		server.Close()
	}
	return results, nil
}

func replay(base string, ex contract.Exchange) (Result, error) {
	result := Result{Procedure: ex.Procedure, WantStatus: ex.Response.Status}
	stream := strings.HasPrefix(ex.Response.ContentType, "text/event-stream")
	var err error
	if stream {
		var frame string
		if err := json.Unmarshal(ex.Response.Body, &frame); err != nil {
			return result, err
		}
		if result.WantBody, err = normalizeFrame(frame); err != nil {
			return result, err
		}
	} else if result.WantBody, err = normalizeJSON(ex.Response.Body); err != nil {
		return result, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := strings.ReplaceAll(string(ex.Request.Body), "<uuid>", RequestUUID)
	method := ex.Request.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, base+ex.Request.Path, strings.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	if stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		frame, err := firstEvent(resp.Body)
		if err != nil {
			return result, err
		}
		result.Body, err = normalizeFrame(frame)
		return result, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return result, err
	}
	if result.Body, err = normalizeJSON(data); err != nil {
		result.Body = string(data)
	}
	return result, nil
}

// firstEvent reads the first event frame, skipping comment frames.
func firstEvent(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 16<<20)
	var frame strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if frame.Len() > 0 {
				return frame.String() + "\n", nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		frame.WriteString(line + "\n")
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return frame.String(), io.ErrUnexpectedEOF
}

func normalizeFrame(frame string) (string, error) {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimRight(frame, "\n"), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			normalized, err := normalizeJSON([]byte(data))
			if err != nil {
				return "", err
			}
			line = "data: " + normalized
		}
		out.WriteString(line + "\n")
	}
	return out.String(), nil
}

var (
	uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	isoTime     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
	timeKey     = regexp.MustCompile(`(At|Time|time|timestamp)$`)
	tempPath    = regexp.MustCompile(`^/tmp/|persona-contract-`)
)

func normalizeJSON(data []byte) (string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "null", nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalize(value, "")); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func normalize(value any, key string) any {
	switch v := value.(type) {
	case []any:
		for i := range v {
			v[i] = normalize(v[i], key)
		}
	case map[string]any:
		for name, item := range v {
			v[name] = normalize(item, name)
		}
	case string:
		switch {
		case uuidPattern.MatchString(v):
			return "<uuid>"
		case isoTime.MatchString(v):
			return "<time>"
		case tempPath.MatchString(v):
			return "<path>"
		}
	case json.Number:
		if f, err := v.Float64(); err == nil && f > 1e12 && timeKey.MatchString(key) {
			return "<time>"
		}
	}
	return value
}

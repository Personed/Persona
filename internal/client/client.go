// Package client calls a Persona service over the oRPC wire in contracts/api.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
)

// SessionCookie is the cookie a paired device presents instead of a Bearer token.
const SessionCookie = "persona_session"

// Client calls one Persona service.
type Client struct {
	// BaseURL is the service origin, such as http://127.0.0.1:3000.
	BaseURL string
	// Token authenticates as a paired device. Empty for an unauthenticated local service.
	Token string
	HTTP  *http.Client
}

// Error is the oRPC error envelope.
type Error struct {
	Defined bool            `json:"defined"`
	Code    string          `json:"code"`
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s (%d)", e.Code, e.Status)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// Code returns the oRPC error code of err, or "" when err is not an *Error.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// URL joins path to the service origin.
func (c *Client) URL(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

// RPCPath maps a procedure path such as "bots.list" to its HTTP path.
func RPCPath(procedure string) string {
	return "/rpc/" + strings.ReplaceAll(procedure, ".", "/")
}

// authorize adds the device token. Mutating requests also carry the service
// origin, which the hosting origin guard requires when there is no token.
func (c *Client) authorize(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if req.Method != http.MethodGet {
		req.Header.Set("Origin", req.URL.Scheme+"://"+req.URL.Host)
	}
}

func (c *Client) post(ctx context.Context, procedure string, input any, accept string) (*http.Response, error) {
	body := []byte("{}")
	if input != nil {
		var err error
		if body, err = json.Marshal(struct {
			JSON any `json:"json"`
		}{input}); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(RPCPath(procedure)), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	c.authorize(req)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()
		return nil, readError(res)
	}
	return res, nil
}

func readError(res *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var envelope struct {
		JSON *Error `json:"json"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.JSON != nil && envelope.JSON.Code != "" {
		return envelope.JSON
	}
	var plain struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &plain) == nil && plain.Error != "" {
		data = []byte(plain.Error)
	}
	return &Error{Code: http.StatusText(res.StatusCode), Status: res.StatusCode, Message: strings.TrimSpace(string(data))}
}

// Call runs a unary procedure. input nil sends no input; output nil discards the result.
func (c *Client) Call(ctx context.Context, procedure string, input, output any) error {
	res, err := c.post(ctx, procedure, input, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var envelope struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("%s: decode response: %w", procedure, err)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.JSON, output); err != nil {
		return fmt.Errorf("%s: decode output: %w", procedure, err)
	}
	return nil
}

// Stream is an open event-stream procedure.
type Stream struct {
	procedure string
	body      io.ReadCloser
	scanner   *bufio.Scanner
}

// Subscribe opens a stream procedure. Close the stream or cancel ctx to end it.
func (c *Client) Subscribe(ctx context.Context, procedure string, input any) (*Stream, error) {
	res, err := c.post(ctx, procedure, input, "text/event-stream")
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	return &Stream{procedure: procedure, body: res.Body, scanner: scanner}, nil
}

// Next decodes the next message into v. It returns io.EOF when the stream ends with done.
func (s *Stream) Next(v any) error {
	for {
		event, data, err := s.event()
		if err != nil {
			return err
		}
		switch event {
		case "message", "":
			var envelope struct {
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return fmt.Errorf("%s: decode event: %w", s.procedure, err)
			}
			if v == nil {
				return nil
			}
			// Reset v so a reused destination does not share slices with earlier events.
			if target := reflect.ValueOf(v); target.Kind() == reflect.Pointer && !target.IsNil() {
				target.Elem().SetZero()
			}
			return json.Unmarshal(envelope.JSON, v)
		case "done":
			return io.EOF
		case "error":
			var envelope struct {
				JSON *Error `json:"json"`
			}
			if json.Unmarshal(data, &envelope) == nil && envelope.JSON != nil {
				return envelope.JSON
			}
			return &Error{Code: "INTERNAL_SERVER_ERROR", Status: 500, Message: string(data)}
		}
	}
}

func (s *Stream) event() (string, []byte, error) {
	var event string
	var data []byte
	seen := false
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if seen {
				return event, data, nil
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event, seen = value, true
		case "data":
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data, seen = append(data, value...), true
		}
	}
	if err := s.scanner.Err(); err != nil {
		return "", nil, err
	}
	if seen {
		return event, data, nil
	}
	return "", nil, io.EOF
}

// Close ends the stream.
func (s *Stream) Close() error { return s.body.Close() }

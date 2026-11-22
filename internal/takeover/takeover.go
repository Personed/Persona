// Package takeover serves the computer_request_takeover product tool, which pauses a
// task and asks the user to operate the Bot's browser or desktop.
//
// It mirrors packages/agent/src/computer/takeover.ts. The tool validates the request;
// the shared computer runtime owns the stored chat card, the viewer lease, and completion.
package takeover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"

	"github.com/Personed/Persona/internal/tools"
)

// Name is the tool name every engine declares.
const Name = "computer_request_takeover"

// ReasonLength bounds the reason, in UTF-16 code units like the TypeScript schema.
const ReasonLength = 1000

// Services the user can take over.
const (
	Browser = "browser"
	Desktop = "desktop"
)

// ErrUnavailable is returned by a Handoff when the Bot has no bound work computer
// with the requested service enabled.
var ErrUnavailable = errors.New("takeover: computer service is not enabled")

// Request is one handoff request. The model supplies only Service and Reason; the
// Bot, task, and call identity come from the call. CallID is stable across
// redelivery, so a Handoff can keep one card per call.
type Request struct {
	BotID   int64
	TaskID  int64
	CallID  string
	Service string
	Reason  string
}

// Handoff posts the stored chat card for a request and holds the call. It reports
// true only when the user explicitly returns control, and false when the request is
// cancelled, denied, or expired. Closing a viewer does not complete a request. It
// returns when ctx ends.
type Handoff func(ctx context.Context, request Request) (returned bool, err error)

// Register adds computer_request_takeover to a registry. The tool does not set
// Mutates: the takeover card is its own confirmation.
func Register(registry tools.Registry, handoff Handoff) error {
	if handoff == nil {
		return errors.New("takeover: register: nil handoff")
	}
	return registry.Register(tool{handoff})
}

var definition = tools.Definition{
	Name:        Name,
	Description: "Pause this task and ask the user to operate the assigned browser or desktop. Use for sign-in, CAPTCHA, sensitive steps, or an explicit user handoff. Wait for completion. Observe fresh state before continuing.",
	InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"service":{"type":"string","enum":[%q,%q]},"reason":{"type":"string","minLength":1,"maxLength":%d}},"required":["service","reason"],"additionalProperties":false}`, Browser, Desktop, ReasonLength)),
}

type tool struct{ handoff Handoff }

func (tool) Definition() tools.Definition { return definition }

func (t tool) Run(ctx context.Context, call tools.Call) (tools.Result, error) {
	request, err := parse(call)
	if err != nil {
		return failure("参数无效：" + err.Error()), nil
	}
	returned, err := t.handoff(ctx, request)
	switch {
	case errors.Is(err, ErrUnavailable):
		return failure("请先启用对应的电脑服务。"), nil
	case err != nil:
		return tools.Result{}, err
	case !returned:
		return failure("Takeover cancelled."), nil
	}
	return tools.Result{Content: []tools.Content{{Type: "text", Text: "User returned control. Observe fresh state before continuing. Do not replay earlier actions."}}}, nil
}

func failure(text string) tools.Result {
	return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: text}}}
}

func parse(call tools.Call) (Request, error) {
	var args struct {
		Service *string `json:"service"`
		Reason  *string `json:"reason"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return Request{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("unexpected data after the arguments")
	}
	if args.Service == nil || (*args.Service != Browser && *args.Service != Desktop) {
		return Request{}, fmt.Errorf("service must be %q or %q", Browser, Desktop)
	}
	if args.Reason == nil {
		return Request{}, errors.New("reason is required")
	}
	reason := strings.TrimSpace(*args.Reason)
	if length := len(utf16.Encode([]rune(reason))); length < 1 || length > ReasonLength {
		return Request{}, fmt.Errorf("reason must have 1 to %d characters", ReasonLength)
	}
	return Request{BotID: call.BotID, TaskID: call.TaskID, CallID: call.CallID, Service: *args.Service, Reason: reason}, nil
}

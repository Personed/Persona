package takeover_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/takeover"
	"github.com/Personed/Persona/internal/tools"
)

func call(t *testing.T, handoff takeover.Handoff, input string) (tools.Result, error) {
	t.Helper()
	registry := tools.NewRegistry()
	if err := takeover.Register(registry, handoff); err != nil {
		t.Fatal(err)
	}
	// A nil approver proves the tool reaches the handoff without the mutation approval.
	return registry.Call(context.Background(), tools.Call{BotID: 7, TaskID: 9, CallID: "s:1", Name: takeover.Name, Input: json.RawMessage(input)}, nil)
}

func text(result tools.Result) string {
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		return ""
	}
	return result.Content[0].Text
}

func TestDefinition(t *testing.T) {
	registry := tools.NewRegistry()
	if err := takeover.Register(registry, func(context.Context, takeover.Request) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	definitions, err := registry.List(context.Background(), 1)
	if err != nil || len(definitions) != 1 || definitions[0].Name != "computer_request_takeover" || definitions[0].Mutates {
		t.Fatalf("List = %+v, %v", definitions, err)
	}
	var schema struct {
		Properties map[string]struct {
			Enum      []string `json:"enum"`
			MaxLength int      `json:"maxLength"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
	}
	if err := json.Unmarshal(definitions[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if strings.Join(schema.Properties["service"].Enum, ",") != "browser,desktop" || schema.Properties["reason"].MaxLength != 1000 ||
		strings.Join(schema.Required, ",") != "service,reason" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("schema = %s", definitions[0].InputSchema)
	}
	if err := takeover.Register(tools.NewRegistry(), nil); err == nil {
		t.Fatal("Register accepted a nil handoff")
	}
}

func TestRun(t *testing.T) {
	failed := errors.New("card storage failed")
	for _, test := range []struct {
		name     string
		returned bool
		err      error
		want     string
		isError  bool
		wantErr  error
	}{
		{name: "returned", returned: true, want: "User returned control. Observe fresh state before continuing. Do not replay earlier actions."},
		{name: "cancelled", want: "Takeover cancelled.", isError: true},
		{name: "unavailable", err: takeover.ErrUnavailable, want: "请先启用对应的电脑服务。", isError: true},
		{name: "failed", err: failed, wantErr: failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got takeover.Request
			result, err := call(t, func(_ context.Context, request takeover.Request) (bool, error) {
				got = request
				return test.returned, test.err
			}, `{"service":"browser","reason":"  Sign in to the bank  "}`)
			if !errors.Is(err, test.wantErr) || (test.wantErr == nil && err != nil) {
				t.Fatalf("err = %v", err)
			}
			if want := (takeover.Request{BotID: 7, TaskID: 9, CallID: "s:1", Service: takeover.Browser, Reason: "Sign in to the bank"}); got != want {
				t.Fatalf("request = %+v", got)
			}
			if text(result) != test.want || result.IsError != test.isError {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestRunRejectsInvalidInput(t *testing.T) {
	// U+1F600 is two UTF-16 code units, so 500 of them fill the limit exactly.
	if result, err := call(t, func(_ context.Context, request takeover.Request) (bool, error) {
		return request.Service == takeover.Desktop, nil
	}, `{"service":"desktop","reason":"`+strings.Repeat("😀", 500)+`"}`); err != nil || result.IsError {
		t.Fatalf("limit-length reason = %+v, %v", result, err)
	}
	// JavaScript trim removes U+FEFF and keeps U+0085, so 999 letters and U+0085 fit.
	if result, err := call(t, func(_ context.Context, request takeover.Request) (bool, error) {
		return request.Reason == "\u0085"+strings.Repeat("a", 999), nil
	}, `{"service":"browser","reason":"\uFEFF\u0085`+strings.Repeat("a", 999)+`\u00a0"}`); err != nil || result.IsError {
		t.Fatalf("JavaScript trim = %+v, %v", result, err)
	}
	for name, input := range map[string]string{
		"empty":           ``,
		"not an object":   `[]`,
		"missing service": `{"reason":"x"}`,
		"unknown service": `{"service":"phone","reason":"x"}`,
		"missing reason":  `{"service":"browser"}`,
		"blank reason":    `{"service":"browser","reason":"  "}`,
		"long reason":     `{"service":"browser","reason":"` + strings.Repeat("😀", 501) + `"}`,
		"extra property":  `{"service":"browser","reason":"x","botId":2}`,
		"capital keys":    `{"Service":"browser","Reason":"x"}`,
		"capital extra":   `{"service":"browser","reason":"x","Reason":"y"}`,
		"null reason":     `{"service":"browser","reason":null}`,
		"number reason":   `{"service":"browser","reason":1}`,
		"null arguments":  `null`,
		"only U+FEFF":     `{"service":"browser","reason":"\uFEFF \u2028"}`,
		"trailing data":   `{"service":"browser","reason":"x"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := call(t, func(context.Context, takeover.Request) (bool, error) {
				t.Error("handoff ran for invalid input")
				return true, nil
			}, input)
			if err != nil || !result.IsError || !strings.HasPrefix(text(result), "参数无效：") {
				t.Fatalf("result = %+v, %v", result, err)
			}
		})
	}
}

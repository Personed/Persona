package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

// Register adds the conversation_read product tool to a registry.
func Register(registry tools.Registry, reader *Reader) error {
	return registry.Register(readTool{reader})
}

type readTool struct{ reader *Reader }

func (readTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "conversation_read",
		Description: "Read quoted Bot history. Pass from as before for older pages. Read an entryId with a UTF-8 byte offset to recover omitted text, selected material, and tool details. Continue with nextOffset. Files remain references on their recorded computer.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"before":{"type":"integer","minimum":1},"entryId":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
		Annotations: map[string]any{"readOnlyHint": true},
	}
}

// maxSafeInteger is the largest cursor the TypeScript service accepts.
const maxSafeInteger = 1<<53 - 1

func cursor(value *float64, minimum float64) bool {
	return value == nil || (*value >= minimum && *value <= maxSafeInteger && *value == math.Trunc(*value))
}

// Run reads the conversation of the calling Bot, whose ID is its conversation ID.
func (t readTool) Run(ctx context.Context, call tools.Call) (tools.Result, error) {
	var args struct {
		Before  *float64 `json:"before"`
		EntryID *float64 `json:"entryId"`
		Offset  *float64 `json:"offset"`
	}
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var text string
	err := decoder.Decode(&args)
	switch {
	case err != nil, !cursor(args.Before, 1), !cursor(args.EntryID, 1), !cursor(args.Offset, 0),
		args.EntryID != nil && args.Before != nil, args.Offset != nil && args.EntryID == nil:
		err = &Error{"聊天记录游标无效。"}
	case args.EntryID != nil:
		var offset int64
		if args.Offset != nil {
			offset = int64(*args.Offset)
		}
		text, err = t.reader.Entry(ctx, store.ConversationID(call.BotID), store.EntryID(*args.EntryID), offset)
	default:
		var before store.EntryID
		if args.Before != nil {
			before = store.EntryID(*args.Before)
		}
		text, err = t.reader.Page(ctx, store.ConversationID(call.BotID), before)
	}
	if failure := (*Error)(nil); errors.As(err, &failure) {
		return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: failure.Message}}}, nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	if text == "" {
		text = "{}"
	}
	return tools.Result{Content: []tools.Content{{Type: "text", Text: text}}}, nil
}

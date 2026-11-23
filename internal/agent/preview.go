package agent

import (
	"context"
	"encoding/json"
	"math"

	"github.com/Personed/Persona/internal/store"
)

const (
	previewPage   = 32
	previewLength = 160
)

type previewItem struct {
	text      string
	timestamp json.RawMessage
}

type engineChatMessage struct {
	Hidden      bool              `json:"hidden"`
	Role        string            `json:"role"`
	Routine     json.RawMessage   `json:"routine"`
	Pending     bool              `json:"pending"`
	Stopped     bool              `json:"stopped"`
	StopReason  string            `json:"stopReason"`
	IsError     bool              `json:"isError"`
	Attachments []json.RawMessage `json:"attachments"`
	Content     []contentPart     `json:"content"`
	Timestamp   json.RawMessage   `json:"timestamp"`
	TurnID      string            `json:"turnId"`
	Delivery    *struct {
		Text string `json:"text"`
	} `json:"delivery"`
}

func entryPreview(entry store.Entry) *previewItem {
	if hiddenEntry(entry.Kind) {
		return nil
	}
	if entry.Kind == EngineMessage {
		var m engineChatMessage
		_ = json.Unmarshal(entry.Data, &m)
		if m.Hidden || (m.Role == "user" && len(m.Routine) > 0 && string(m.Routine) != "null") ||
			m.Pending || m.Stopped || m.StopReason == "error" {
			return nil
		}
		if (m.Delivery != nil || (m.Role == "toolResult" && len(m.Attachments) > 0)) && !m.IsError {
			text := ""
			if m.Delivery != nil {
				text = m.Delivery.Text
			}
			return &previewItem{previewText(text), m.Timestamp}
		}
		if m.Role == "user" || m.Role == "assistant" {
			text := partsText(m.Content)
			image := false
			for _, part := range m.Content {
				image = image || part.Type == "image"
			}
			if m.Role == "user" || trimJS(text) != "" || len(m.Attachments) > 0 || image {
				if m.Role == "assistant" {
					text = previewText(text)
				}
				return &previewItem{text, m.Timestamp}
			}
		}
		return nil
	}
	messages := decodeMessages(entry.Model)
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		switch {
		case m.Role == "user":
			var input struct {
				Text *string `json:"text"`
			}
			if entry.Kind == FileInput || entry.Kind == RoutineInput {
				_ = json.Unmarshal(entry.Data, &input)
			}
			if input.Text != nil {
				return &previewItem{*input.Text, m.Timestamp}
			}
			return &previewItem{partsText(m.parts()), m.Timestamp}
		case m.Role == "assistant" && m.StopReason != "error" && m.StopReason != "aborted":
			if text := partsText(m.parts()); trimJS(text) != "" {
				return &previewItem{previewText(text), m.Timestamp}
			}
		case m.Role == "toolResult" && !m.IsError:
			var details struct {
				Delivery *struct {
					Text string `json:"text"`
				} `json:"delivery"`
				Attachment json.RawMessage `json:"attachment"`
			}
			_ = json.Unmarshal(m.Details, &details)
			if (details.Delivery != nil || len(details.Attachment) > 0 && string(details.Attachment) != "null") && !aborted(entry) {
				text := ""
				if details.Delivery != nil {
					text = details.Delivery.Text
				}
				return &previewItem{previewText(text), m.Timestamp}
			}
		}
	}
	return nil
}

func aborted(entry store.Entry) bool {
	var data struct {
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	_ = json.Unmarshal(entry.Data, &data)
	for _, d := range data.Diagnostics {
		if d.Code == "aborted" {
			return true
		}
	}
	return false
}

func excerpt(text string) string {
	runes := []rune(trimJS(text))
	if len(runes) > previewLength {
		runes = runes[:previewLength]
	}
	return string(runes)
}

// botPreview is the newest chat text of a conversation, or its instructions,
// and the time of its newest message, as botPreview in bot-preview.ts.
func botPreview(ctx context.Context, records store.Store, id int64, instructions string) (string, *float64, error) {
	var latest *float64
	seen := false
	scan := store.Scan{Limit: previewPage, Descending: true}
	for {
		page, err := records.Entries(ctx, store.EntryQuery{ConversationID: store.ConversationID(id)}, scan)
		if err != nil {
			return "", nil, err
		}
		for _, entry := range page.Items {
			item := entryPreview(entry)
			if item == nil {
				continue
			}
			if !seen {
				seen = true
				var at float64
				if json.Unmarshal(item.timestamp, &at) == nil && !math.IsInf(at, 0) && !math.IsNaN(at) {
					latest = &at
				}
			}
			if text := trimJS(item.text); text != "" {
				return excerpt(text), latest, nil
			}
		}
		if page.Next == "" {
			return excerpt(instructions), latest, nil
		}
		scan.Cursor = page.Next
	}
}

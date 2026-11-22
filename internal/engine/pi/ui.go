package pi

import (
	"encoding/json"
	"strings"

	"github.com/Personed/Persona/internal/engine"
)

// ComputerOperation is the confirm title pi-tools.mjs uses for its bash, write
// and edit approval hook; Persona answers it through the shared computer policy.
const ComputerOperation = "Computer operation"

type uiRequest struct {
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	Title       string   `json:"title"`
	Message     string   `json:"message"`
	Options     []string `json:"options"`
	Prefill     *string  `json:"prefill"`
	Placeholder string   `json:"placeholder"`
	StatusKey   string   `json:"statusKey"`
	StatusText  *string  `json:"statusText"`
	WidgetKey   string   `json:"widgetKey"`
	WidgetLines []string `json:"widgetLines"`
	Text        string   `json:"text"`
	Timeout     *int64   `json:"timeout"`
}

// IsDialog reports whether an extension_ui_request waits for an
// extension_ui_response (select, confirm, input, editor).
func IsDialog(frame json.RawMessage) bool {
	var r uiRequest
	if json.Unmarshal(frame, &r) != nil {
		return false
	}
	switch r.Method {
	case "select", "confirm", "input", "editor":
		return true
	}
	return false
}

// extensionUI maps an extension_ui_request to a runtime notice or an approval
// card, as harness/providers/pi-ui.ts does.
func extensionUI(frame json.RawMessage) (string, map[string]any, bool) {
	var r uiRequest
	if json.Unmarshal(frame, &r) != nil || r.ID == "" {
		return "", nil, false
	}
	notice := func(key, text string) (string, map[string]any, bool) {
		return "runtime", map[string]any{"state": map[string]any{"notices": map[string]string{key: text}}}, true
	}
	switch r.Method {
	case "notify":
		return notice(r.ID, r.Message)
	case "setStatus":
		text := ""
		if r.StatusText != nil {
			text = *r.StatusText
		}
		return notice(r.StatusKey, text)
	case "setWidget":
		return notice(r.WidgetKey, strings.Join(r.WidgetLines, "\n"))
	case "setTitle":
		return notice(r.ID, r.Title)
	case "set_editor_text":
		return "runtime", map[string]any{"state": map[string]any{"editorText": r.Text}}, true
	case "confirm":
		approval := map[string]any{"id": r.ID, "title": r.Title, "detail": r.Message, "kind": "permission"}
		if r.Title == ComputerOperation {
			approval["automatic"] = true
		}
		return "approval", map[string]any{"approval": approval}, true
	case "select", "input", "editor":
		question := map[string]any{"id": r.ID, "title": r.Title}
		switch r.Method {
		case "select":
			options := make([]map[string]string, 0, len(r.Options))
			for _, value := range r.Options {
				options = append(options, map[string]string{"id": value, "label": value})
			}
			question["allowOther"] = true
			question["options"] = options
		case "editor":
			value := ""
			if r.Prefill != nil {
				value = *r.Prefill
			}
			question["value"] = value
			question["multiline"] = true
		}
		approval := map[string]any{
			"id": r.ID, "title": r.Title, "detail": "", "kind": "question",
			"questions": []map[string]any{question},
		}
		return "approval", map[string]any{"approval": approval}, true
	}
	return "", nil, false
}

// Answer is Persona's answer to an approval card (EngineAnswer in harness/types.ts).
type Answer struct {
	Allow   bool                `json:"allow"`
	Answers map[string][]string `json:"answers,omitempty"`
	Text    string              `json:"text,omitempty"`
}

// UIResponse is the extension_ui_response for a dialog request and Persona's
// answer: cancelled when not allowed, confirmed for confirm, else the value.
func UIResponse(request json.RawMessage, answer Answer) (map[string]any, error) {
	var r uiRequest
	if err := json.Unmarshal(request, &r); err != nil {
		return nil, err
	}
	response := map[string]any{"type": "extension_ui_response", "id": r.ID}
	switch {
	case !answer.Allow:
		response["cancelled"] = true
	case r.Method == "confirm":
		response["confirmed"] = true
	default:
		value := answer.Text
		if values := answer.Answers[r.ID]; len(values) > 0 {
			value = values[0]
		}
		response["value"] = value
	}
	return response, nil
}

// ApprovalClosed is the event that retires an approval card once it is
// answered, cancelled or timed out.
func ApprovalClosed(id string) engine.Event {
	return event("approval_closed", map[string]any{"id": id})
}

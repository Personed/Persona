package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const ecmaSpace = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

// trimJS is String.prototype.trim.
func trimJS(s string) string { return strings.Trim(s, ecmaSpace) }

// modelMessage is a pi-ai Message as entries store it.
type modelMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Timestamp    json.RawMessage `json:"timestamp,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
	Details      json.RawMessage `json:"details,omitempty"`
	Diagnostics  []struct {
		Type    string `json:"type"`
		Details struct {
			Requests []struct {
				Status int `json:"status"`
				Causes []struct {
					Code string `json:"code"`
				} `json:"causes"`
			} `json:"requests"`
		} `json:"details"`
	} `json:"diagnostics,omitempty"`
}

type contentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Redacted  bool            `json:"redacted"`
	Data      string          `json:"data"`
	MimeType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// parts returns the content parts; string content is one text part.
func (m modelMessage) parts() []contentPart {
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		return []contentPart{{Type: "text", Text: text}}
	}
	var parts []contentPart
	_ = json.Unmarshal(m.Content, &parts)
	return parts
}

func partsText(parts []contentPart) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func decodeMessages(raw json.RawMessage) []modelMessage {
	var messages []modelMessage
	_ = json.Unmarshal(raw, &messages)
	return messages
}

// modelFailure is the chat text of a failed model reply, as modelFailure in
// packages/agent/src/runtime/model-diagnostics.ts.
func modelFailure(m modelMessage, reference string) string {
	status, code := 0, ""
	for i := len(m.Diagnostics) - 1; i >= 0; i-- {
		if m.Diagnostics[i].Type != "persona.model-request" {
			continue
		}
		if requests := m.Diagnostics[i].Details.Requests; len(requests) > 0 {
			request := requests[len(requests)-1]
			status = request.Status
			for j := len(request.Causes) - 1; j >= 0; j-- {
				if request.Causes[j].Code != "" {
					code = request.Causes[j].Code
					break
				}
			}
		}
		break
	}
	var summary string
	switch {
	case status == 401:
		summary = "模型账号认证失败"
	case status == 403:
		summary = "模型服务拒绝访问"
	case status == 429:
		summary = "模型服务限流"
	case status >= 500:
		summary = "模型服务暂时不可用"
	case m.ErrorMessage == "Connection error." || code != "":
		summary = "连接失败，原因待确认"
	default:
		summary = "回复失败，原因待确认"
	}
	if code != "" {
		summary += fmt.Sprintf("（%s）", code)
	}
	return summary + "〔" + reference + "〕"
}

var (
	markdownImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	markdownLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markdownAutolink = regexp.MustCompile(`<((?:https?|mailto):[^>\s]+)>`)
	markdownHTML     = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	markdownBlock    = regexp.MustCompile(`^\s{0,3}(?:#{1,6}\s+|>\s?|[-*+]\s+(?:\[[ xX]\]\s+)?|\d+[.)]\s+)`)
	markdownRule     = regexp.MustCompile(`^\s{0,3}(?:(?:[-*_]\s*){3,}|\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?)$`)
	markdownFence    = regexp.MustCompile("^\\s{0,3}(?:```|~~~)")
	markdownMarks    = regexp.MustCompile("\\*+|~~|`+|(^|[\\s(])_+|_+([\\s).,!?:;]|$)")
	whitespace       = regexp.MustCompile(`\s+`)
)

// previewText is the plain text of Markdown for chat previews. It approximates
// previewText in packages/agent/src/preview-text.ts (a GFM parse) for the
// constructs chat replies use.
func previewText(value string) string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if markdownFence.MatchString(line) || markdownRule.MatchString(line) {
			continue
		}
		for {
			stripped := markdownBlock.ReplaceAllString(line, "")
			if stripped == line {
				break
			}
			line = stripped
		}
		lines = append(lines, strings.ReplaceAll(line, "|", " "))
	}
	text := strings.Join(lines, " ")
	text = markdownImage.ReplaceAllString(text, "$1")
	text = markdownLink.ReplaceAllString(text, "$1")
	text = markdownAutolink.ReplaceAllString(text, "$1")
	text = markdownHTML.ReplaceAllString(text, "")
	text = markdownMarks.ReplaceAllString(text, "$1$2")
	return strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
}

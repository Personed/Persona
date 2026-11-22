package tui

import (
	"bytes"
	"encoding/json"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Personed/Persona/internal/client"
)

var (
	userStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	botStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
)

// renderMessages renders the visible chat transcript at width columns.
func renderMessages(view client.ChatView, bot string, width int) string {
	wrap := lipgloss.NewStyle().Width(max(width, 20))
	var blocks []string
	for _, m := range view.Messages {
		if m.Hidden {
			continue
		}
		var lines []string
		switch {
		case m.Notice != nil:
			lines = append(lines, faint.Render("— "+m.Notice.Text+" —"))
		case m.Role == "user":
			lines = append(lines, userStyle.Render("You"))
			if m.Routine != nil {
				lines[0] += faint.Render(" · routine " + m.Routine.Title)
			}
			lines = append(lines, wrap.Render(m.Text()))
		case m.Role == "toolResult":
			line := "  ↳ " + m.ToolName
			if m.ToolStatus != "" {
				line += " " + m.ToolStatus
			}
			lines = append(lines, faint.Render(line))
		default:
			lines = append(lines, botStyle.Render(bot))
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						lines = append(lines, wrap.Render(c.Text))
					}
				case "thinking":
					if c.Thinking != "" {
						lines = append(lines, faint.Render(wrap.Render(c.Thinking)))
					}
				case "toolCall":
					lines = append(lines, faint.Render("  ⏺ "+c.Name+" "+truncate(compact(c.Arguments), max(width-len(c.Name)-6, 0))))
				case "image":
					lines = append(lines, faint.Render("  [image]"))
				}
			}
			if m.Approval != nil {
				lines = append(lines, faint.Render("  ["+m.Approval.State+"] "+m.Approval.Request.Title))
			}
			if m.Pending {
				lines = append(lines, faint.Render("…"))
			}
		}
		if m.Error != "" {
			lines = append(lines, errorStyle.Render(wrap.Render(m.Error)))
		}
		if m.Stopped {
			lines = append(lines, faint.Render("(stopped)"))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	if view.Error != "" {
		blocks = append(blocks, errorStyle.Render(wrap.Render(view.Error)))
	}
	return strings.Join(blocks, "\n\n")
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// truncate cuts s to n terminal cells.
func truncate(s string, n int) string {
	return ansi.Truncate(s, max(n, 0), "…")
}

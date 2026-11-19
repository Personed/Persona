package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// answer is the in-progress reply to the first pending approval of a chat.
type answer struct {
	request  client.Approval
	question int
	cursor   int
	picked   map[string][]string
	typing   bool
}

func (a *answer) options() []client.Option {
	if len(a.request.Questions) > 0 {
		return a.request.Questions[a.question].Options
	}
	if len(a.request.Options) > 0 {
		return a.request.Options
	}
	confirm := a.request.ConfirmLabel
	if confirm == "" {
		confirm = "Allow"
	}
	return []client.Option{{ID: "allow", Label: confirm}, {ID: "deny", Label: "Deny"}}
}

// syncAnswer starts an answer for the first pending approval, keeping one in progress.
func (c *chat) syncAnswer() {
	pending := c.view.Approvals
	if c.answer != nil && slices.ContainsFunc(pending, func(a client.Approval) bool { return a.ID == c.answer.request.ID }) {
		return
	}
	c.answer = nil
	if len(pending) > 0 {
		c.answer = &answer{request: pending[0], picked: map[string][]string{}}
	}
}

func (m Model) answerCmd(input map[string]any) tea.Cmd {
	input["botId"] = m.chat.bot.ID
	input["id"] = m.chat.answer.request.ID
	m.chat.answer = nil
	return m.call("engines.answer", input)
}

// answerKey handles keys while the approval card has focus.
func (m Model) answerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c, a := m.chat, m.chat.answer
	options := a.options()
	switch msg.String() {
	case "tab":
		a.typing = true
		return m, c.input.Focus()
	case "up", "k":
		a.cursor = max(a.cursor-1, 0)
	case "down", "j":
		a.cursor = min(a.cursor+1, max(len(options)-1, 0))
	case "ctrl+n":
		return m, m.answerCmd(map[string]any{"allow": false})
	case "y":
		if len(a.request.Questions) == 0 {
			return m, m.answerCmd(map[string]any{"allow": true})
		}
	case "n":
		if len(a.request.Questions) == 0 {
			return m, m.answerCmd(map[string]any{"allow": false})
		}
	case "space":
		if q := a.current(); q != nil && q.Multiple && len(options) > 0 {
			id := options[a.cursor].ID
			if i := slices.Index(a.picked[q.ID], id); i >= 0 {
				a.picked[q.ID] = slices.Delete(a.picked[q.ID], i, i+1)
			} else {
				a.picked[q.ID] = append(a.picked[q.ID], id)
			}
		}
	case "enter":
		return m.confirm("")
	}
	return m, nil
}

func (a *answer) current() *client.Question {
	if len(a.request.Questions) == 0 {
		return nil
	}
	return &a.request.Questions[a.question]
}

// confirm records the selected option, or text when given, and submits after the last question.
func (m Model) confirm(text string) (tea.Model, tea.Cmd) {
	a := m.chat.answer
	options := a.options()
	q := a.current()
	if q == nil {
		if text != "" {
			return m, m.answerCmd(map[string]any{"allow": true, "text": text})
		}
		choice := options[a.cursor]
		switch {
		case choice.ID == "deny" && len(a.request.Options) == 0:
			return m, m.answerCmd(map[string]any{"allow": false})
		case len(a.request.Options) == 0:
			return m, m.answerCmd(map[string]any{"allow": true})
		default:
			return m, m.answerCmd(map[string]any{"allow": true, "optionId": choice.ID})
		}
	}
	switch {
	case text != "":
		a.picked[q.ID] = append(a.picked[q.ID], text)
	case !q.Multiple && len(options) > 0:
		a.picked[q.ID] = []string{options[a.cursor].ID}
	}
	if q.Required && len(a.picked[q.ID]) == 0 {
		return m, nil
	}
	if a.question+1 < len(a.request.Questions) {
		a.question, a.cursor = a.question+1, 0
		return m, nil
	}
	answers := map[string][]string{}
	for _, q := range a.request.Questions {
		answers[q.ID] = append([]string{}, a.picked[q.ID]...)
	}
	return m, m.answerCmd(map[string]any{"allow": true, "answers": answers})
}

func (m Model) answerView() string {
	a := m.chat.answer
	var b strings.Builder
	title := a.request.Title
	if q := a.current(); q != nil {
		title = q.Title
		if len(a.request.Questions) > 1 {
			title += faint.Render(" (" + itoa(a.question+1) + "/" + itoa(len(a.request.Questions)) + ")")
		}
	}
	b.WriteString(approvalStyle.Render("? "+title) + "\n")
	if a.request.Detail != "" && a.question == 0 {
		b.WriteString(truncate(firstLine(a.request.Detail), m.width) + "\n")
	}
	for _, f := range a.request.Fields {
		b.WriteString(faint.Render(f.Label+": ") + truncate(f.Value, m.width-len(f.Label)-2) + "\n")
	}
	q := a.current()
	for i, o := range a.options() {
		mark := ""
		if q != nil && q.Multiple {
			mark = "[ ] "
			if slices.Contains(a.picked[q.ID], o.ID) {
				mark = "[x] "
			}
		}
		line := mark + o.Label
		if o.Description != "" {
			line += faint.Render(" — " + o.Description)
		}
		if i == a.cursor && !a.typing {
			line = selected.Render("> " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m Model) answerHelp() string {
	if m.chat.answer.typing {
		return "enter answer with text · tab choose option · "
	}
	if m.chat.answer.current() == nil {
		return "y allow · n deny · ↑/↓ + enter choose · tab reply with text · "
	}
	return "↑/↓ + enter choose · space toggle · tab type answer · ctrl+n decline · "
}

func itoa(n int) string { return strconv.Itoa(n) }

package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// work is the Work screen of one Bot: routines, recent runs, memory and skills.
type work struct {
	bot    client.Bot
	data   client.Work
	loaded bool
	cursor int
	cancel context.CancelFunc
	back   screen
	// chat is the chat Work was opened from, kept with its draft and transcript until esc returns to it.
	chat *chat
	// pending is the routine action whose outcome is unknown; repeating it reuses its requestId.
	pending, pendingID string
}

type routineMsg struct {
	botID int
	key   string
	err   error
}

type workStreamMsg struct {
	botID  int
	stream *client.Stream
	err    error
}

type workMsg struct {
	botID  int
	stream *client.Stream
	work   client.Work
	err    error
}

func nextWork(botID int, stream *client.Stream) tea.Cmd {
	return func() tea.Msg {
		var w client.Work
		err := stream.Next(&w)
		return workMsg{botID, stream, w, err}
	}
}

func (m Model) openWork(bot client.Bot, from *chat) (tea.Model, tea.Cmd) {
	m.closeWork()
	ctx, cancel := context.WithCancel(m.ctx)
	m.work = &work{bot: bot, cancel: cancel, back: m.screen, chat: from}
	m.screen, m.err = screenWork, nil
	c := m.client
	return m, func() tea.Msg {
		stream, err := c.Subscribe(ctx, "work.subscribe", map[string]int{"botId": bot.ID})
		return workStreamMsg{bot.ID, stream, err}
	}
}

func (m *Model) closeWork() {
	if m.work != nil {
		m.work.cancel()
		m.work = nil
	}
}

func (m Model) updateWork(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case workStreamMsg:
		if m.work == nil || m.work.bot.ID != msg.botID {
			if msg.stream != nil {
				_ = msg.stream.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		return m, nextWork(msg.botID, msg.stream)
	case workMsg:
		if m.work == nil || m.work.bot.ID != msg.botID {
			return m, nil
		}
		if msg.err != nil {
			if !isEnd(msg.err) {
				m.err = msg.err
			}
			return m, nil
		}
		m.work.data, m.work.loaded = msg.work, true
		m.work.cursor = min(m.work.cursor, max(len(msg.work.Routines.Items)-1, 0))
		return m, nextWork(msg.botID, msg.stream)
	case routineMsg:
		m.err = msg.err
		if w := m.work; w != nil && w.bot.ID == msg.botID && w.pending == msg.key && (msg.err == nil || rejected(msg.err)) {
			w.pending, w.pendingID = "", ""
		}
	}
	return m, nil
}

// routine runs a routine action, reusing the requestId of the same action while its outcome is unknown.
func (m Model) routine(procedure, key string, input map[string]any) tea.Cmd {
	w := m.work
	if w.pending != key {
		w.pending, w.pendingID = key, requestID()
	}
	input["botId"], input["requestId"] = w.bot.ID, w.pendingID
	c, ctx, id := m.client, m.ctx, w.bot.ID
	return func() tea.Msg { return routineMsg{id, key, c.Call(ctx, procedure, input, nil)} }
}

// resumeChat returns to a chat kept while Work was open and reopens its changes stream.
func (m Model) resumeChat(c *chat) (tea.Model, tea.Cmd) {
	c.ctx, c.cancel = context.WithCancel(m.ctx)
	m.chat, m.screen, m.err = c, screenChat, nil
	m.layout()
	return m, tea.Batch(c.input.Focus(), m.subscribe())
}

func (m Model) workKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	w := m.work
	routines := w.data.Routines.Items
	switch msg.String() {
	case "esc", "q":
		back, bot, kept := w.back, w.bot, w.chat
		m.closeWork()
		m.screen, m.err = back, nil
		if back == screenChat && m.chat == nil {
			if kept != nil {
				return m.resumeChat(kept)
			}
			return m.openChat(bot)
		}
		if back == screenBots {
			return m, m.loadBots()
		}
	case "up", "k":
		w.cursor = max(w.cursor-1, 0)
	case "down", "j":
		w.cursor = min(w.cursor+1, max(len(routines)-1, 0))
	case "e":
		if len(routines) > 0 {
			r := routines[w.cursor]
			return m, m.routine("work.routines.enable", fmt.Sprintf("enable %s %d %v", r.ID, r.Revision, !r.Enabled), map[string]any{
				"id": r.ID, "revision": r.Revision, "enabled": !r.Enabled,
			})
		}
	case "t":
		if len(routines) > 0 {
			id := routines[w.cursor].ID
			return m, m.routine("work.routines.test", "test "+id, map[string]any{"id": id})
		}
	}
	return m, nil
}

const recentRuns = 8

func (m Model) workView() string {
	w := m.work
	var b strings.Builder
	b.WriteString(titleStyle.Render(w.bot.Name+" · Work") + "\n\n")
	if !w.loaded {
		b.WriteString(faint.Render("loading…") + "\n")
		return b.String() + m.errLine()
	}
	rest := m.workRest()
	b.WriteString(heading.Render("Routines") + "\n")
	items := w.data.Routines.Items
	if len(items) == 0 {
		b.WriteString(faint.Render("  No routines. Ask the Bot in chat to create one.") + "\n")
	}
	// Show the window of routines around the cursor that fits above the other sections.
	rows := max(m.height-3-strings.Count(rest, "\n")-1, 1)
	start := min(max(w.cursor-rows+1, 0), max(len(items)-rows, 0))
	for i := start; i < min(start+rows, len(items)); i++ {
		r := items[i]
		state := "on "
		if !r.Enabled {
			state = "off"
		}
		line := fmt.Sprintf("%s  %s  %s", state, r.Title, faint.Render(scheduleLabel(r.Schedule)))
		if next := millis(r.NextAt); r.Enabled && next != "" {
			line += faint.Render(" · next " + next)
		}
		if i == w.cursor {
			b.WriteString(selected.Render("> "+truncate(line, m.width-2)) + "\n")
		} else {
			b.WriteString("  " + truncate(line, m.width-2) + "\n")
		}
	}
	return b.String() + rest
}

// workRest renders the sections below the routines: recent runs, memory and skills, and help.
func (m Model) workRest() string {
	w := m.work
	var b strings.Builder
	b.WriteString("\n" + heading.Render("Recent runs") + "\n")
	runs := slices.Clone(w.data.Routines.Runs)
	slices.Reverse(runs)
	if len(runs) == 0 {
		b.WriteString(faint.Render("  No runs yet.") + "\n")
	}
	for _, r := range runs[:min(len(runs), recentRuns)] {
		line := fmt.Sprintf("  %-16s %s %s", runStatus(r.Status), r.Title, faint.Render(r.Mode+" "+millis(r.FiredAt)))
		if r.Error != "" {
			line += " " + errorStyle.Render(firstLine(r.Error))
		}
		b.WriteString(truncate(line, m.width) + "\n")
	}
	b.WriteString("\n" + heading.Render("Memory and skills") + "\n")
	review := "on"
	if !w.data.MemoryReviewEnabled {
		review = "off"
	}
	enabled := 0
	for _, s := range w.data.Skills {
		if slices.Contains(s.EnabledBots, w.bot.ID) {
			enabled++
		}
	}
	fmt.Fprintf(&b, "  %d memories · review %s · %d of %d skills enabled\n", len(w.data.Memory), review, enabled, len(w.data.Skills))
	b.WriteString("\n" + m.errLine())
	b.WriteString(faint.Render("↑/↓ select · e enable/disable · t test run · esc back"))
	return b.String()
}

func runStatus(status string) string {
	switch status {
	case "done":
		return "✓ done"
	case "failed":
		return errorStyle.Render("✗ failed")
	case "waiting_answer", "waiting_approval":
		return approvalStyle.Render("? " + strings.ReplaceAll(status, "_", " "))
	default:
		return "• " + status
	}
}

// scheduleLabel describes the cron shapes the web UI creates and falls back to the expression.
func scheduleLabel(s client.Schedule) string {
	if s.Type == "once" {
		if at, err := time.Parse(time.RFC3339, s.At); err == nil {
			return "once " + at.Local().Format("Jan 2 15:04")
		}
		return "once " + s.At
	}
	var minute, hour int
	var days string
	if n, _ := fmt.Sscanf(s.Expression, "%d %d * * %s", &minute, &hour, &days); n == 3 {
		names := map[string]string{"*": "daily", "1-5": "weekdays", "0": "Sundays", "1": "Mondays", "2": "Tuesdays", "3": "Wednesdays", "4": "Thursdays", "5": "Fridays", "6": "Saturdays"}
		if name, ok := names[days]; ok {
			return fmt.Sprintf("%s %02d:%02d", name, hour, minute)
		}
	}
	return s.Expression
}

// millis formats an epoch-milliseconds JSON number as local time; other values render empty.
func millis(raw json.RawMessage) string {
	var ms int64
	if json.Unmarshal(raw, &ms) != nil || ms == 0 {
		return ""
	}
	t := time.UnixMilli(ms).Local()
	if time.Since(t).Abs() < 24*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

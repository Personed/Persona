package tui

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Personed/Persona/internal/client"
)

type screen int

const (
	screenBots screen = iota
	screenChat
)

// Model is the root Bubble Tea model.
type Model struct {
	ctx    context.Context
	client *client.Client
	width  int
	height int
	screen screen
	bots   []client.Bot
	cursor int
	err    error
	chat   *chat
}

type chat struct {
	bot      client.Bot
	view     client.ChatView
	loaded   bool
	cancel   context.CancelFunc
	input    textarea.Model
	viewport viewport.Model
}

type botsMsg struct {
	bots []client.Bot
	err  error
}

type streamMsg struct {
	botID  int
	stream *client.Stream
	err    error
}

type changeMsg struct {
	botID  int
	stream *client.Stream
	change client.Change
	err    error
}

type doneMsg struct{ err error }

// New returns the TUI model for the service behind c.
func New(ctx context.Context, c *client.Client) Model {
	return Model{ctx: ctx, client: c, width: 80, height: 24}
}

// Init loads the Bots list.
func (m Model) Init() tea.Cmd { return m.loadBots() }

func (m Model) loadBots() tea.Cmd {
	return func() tea.Msg {
		var bots []client.Bot
		err := m.client.Call(m.ctx, "bots.list", nil, &bots)
		return botsMsg{bots, err}
	}
}

func (m Model) call(procedure string, input any) tea.Cmd {
	return func() tea.Msg { return doneMsg{m.client.Call(m.ctx, procedure, input, nil)} }
}

func next(botID int, stream *client.Stream) tea.Cmd {
	return func() tea.Msg {
		var change client.Change
		err := stream.Next(&change)
		return changeMsg{botID, stream, change, err}
	}
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case botsMsg:
		m.err = msg.err
		if msg.err == nil {
			m.bots = msg.bots
			m.cursor = min(m.cursor, max(len(m.bots)-1, 0))
		}
		return m, nil
	case streamMsg:
		if m.chat == nil || m.chat.bot.ID != msg.botID {
			if msg.stream != nil {
				_ = msg.stream.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		return m, next(msg.botID, msg.stream)
	case changeMsg:
		if m.chat == nil || m.chat.bot.ID != msg.botID {
			return m, nil
		}
		if msg.err != nil {
			if !errors.Is(msg.err, io.EOF) && !errors.Is(msg.err, context.Canceled) {
				m.err = msg.err
			}
			return m, nil
		}
		if err := m.chat.view.Apply(msg.change); err != nil {
			m.err = err
		}
		m.chat.loaded = true
		m.refresh()
		return m, next(msg.botID, msg.stream)
	case doneMsg:
		m.err = msg.err
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.closeChat()
			return m, tea.Quit
		}
		if m.screen == screenChat {
			return m.chatKey(msg)
		}
		return m.botsKey(msg)
	case tea.MouseWheelMsg:
		if m.chat != nil {
			var cmd tea.Cmd
			m.chat.viewport, cmd = m.chat.viewport.Update(msg)
			return m, cmd
		}
	}
	if m.chat != nil {
		var cmd tea.Cmd
		m.chat.input, cmd = m.chat.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) botsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, max(len(m.bots)-1, 0))
	case "r":
		return m, m.loadBots()
	case "enter":
		if len(m.bots) > 0 {
			return m.openChat(m.bots[m.cursor])
		}
	}
	return m, nil
}

func (m Model) openChat(bot client.Bot) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(m.ctx)
	input := textarea.New()
	input.Placeholder = "Message " + bot.Name
	input.ShowLineNumbers = false
	input.Prompt = "> "
	input.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	m.chat = &chat{bot: bot, cancel: cancel, input: input, viewport: viewport.New()}
	m.screen, m.err = screenChat, nil
	m.layout()
	c, id := m.client, bot.ID
	open := func() tea.Msg {
		stream, err := c.Subscribe(ctx, "changes", map[string]int{"botId": id})
		return streamMsg{id, stream, err}
	}
	return m, tea.Batch(m.chat.input.Focus(), open)
}

func (m *Model) closeChat() {
	if m.chat != nil {
		m.chat.cancel()
		m.chat = nil
	}
}

func (m Model) chatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c := m.chat
	switch msg.String() {
	case "esc":
		m.closeChat()
		m.screen, m.err = screenBots, nil
		return m, m.loadBots()
	case "enter":
		text := strings.TrimSpace(c.input.Value())
		if text == "" {
			return m, nil
		}
		c.input.Reset()
		return m, m.call("send", map[string]any{"botId": c.bot.ID, "text": text, "requestId": requestID()})
	case "ctrl+x":
		if c.view.Busy {
			return m, m.call("stop", map[string]int{"botId": c.bot.ID})
		}
		return m, nil
	case "pgup":
		c.viewport.PageUp()
		return m, nil
	case "pgdown":
		c.viewport.PageDown()
		return m, nil
	}
	var cmd tea.Cmd
	c.input, cmd = c.input.Update(msg)
	return m, cmd
}

const inputHeight = 3

func (m *Model) layout() {
	if m.chat == nil {
		return
	}
	m.chat.input.SetWidth(m.width)
	m.chat.input.SetHeight(inputHeight)
	m.chat.viewport.SetWidth(m.width)
	m.chat.viewport.SetHeight(max(m.height-inputHeight-3, 1))
	m.refresh()
}

func (m *Model) refresh() {
	c := m.chat
	follow := c.viewport.AtBottom()
	c.viewport.SetContent(renderMessages(c.view, c.bot.Name, m.width))
	if follow {
		c.viewport.GotoBottom()
	}
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	faint      = lipgloss.NewStyle().Faint(true)
	selected   = lipgloss.NewStyle().Reverse(true)
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// View renders the current screen.
func (m Model) View() tea.View {
	var body string
	if m.screen == screenChat {
		body = m.chatView()
	} else {
		body = m.botsView()
	}
	view := tea.NewView(body)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "Persona"
	return view
}

func (m Model) botsView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Persona · Bots") + "  " + faint.Render(m.client.BaseURL) + "\n\n")
	if len(m.bots) == 0 && m.err == nil {
		b.WriteString(faint.Render("No Bots yet.") + "\n")
	}
	for i, bot := range m.bots {
		line := bot.Name
		if bot.Busy {
			line += " · working"
		}
		if bot.Preview != "" {
			line += faint.Render("  " + truncate(firstLine(bot.Preview), max(m.width-lipgloss.Width(line)-4, 0)))
		}
		if i == m.cursor {
			line = selected.Render("> " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + m.errLine() + faint.Render("↑/↓ select · enter open · r refresh · q quit"))
	return b.String()
}

func (m Model) chatView() string {
	c := m.chat
	header := titleStyle.Render(c.bot.Name)
	if c.view.Model != nil {
		header += faint.Render(" · " + c.view.Model.ModelID)
	}
	switch {
	case !c.loaded:
		header += faint.Render(" · loading")
	case c.view.Busy:
		header += " · working"
	}
	help := "enter send · ctrl+j newline · pgup/pgdn scroll · esc Bots · ctrl+c quit"
	if c.view.Busy {
		help = "ctrl+x stop · " + help
	}
	return header + "\n" + c.viewport.View() + "\n" + c.input.View() + "\n" + m.errLine() + faint.Render(help)
}

func (m Model) errLine() string {
	if m.err == nil {
		return ""
	}
	return errorStyle.Render(m.err.Error()) + "\n"
}

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

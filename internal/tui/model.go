package tui

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
	screenWork
	screenLogin
)

// Model is the root Bubble Tea model.
type Model struct {
	ctx     context.Context
	client  *client.Client
	width   int
	height  int
	screen  screen
	bots    []client.Bot
	cursor  int
	err     error
	chat    *chat
	work    *work
	login   *login
	servers *servers
}

type chat struct {
	ctx      context.Context
	bot      client.Bot
	view     client.ChatView
	loaded   bool
	cancel   context.CancelFunc
	input    textarea.Model
	viewport viewport.Model
	// retryText and retryID keep the request ID of a send whose outcome is unknown,
	// so sending the same text again cannot create a second message.
	retryText string
	retryID   string
	older     bool
	answer    *answer
	picker    *picker
	saving    bool
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

type reconnectMsg struct{ botID int }

type sendMsg struct {
	botID     int
	text      string
	requestID string
	err       error
}

type olderMsg struct {
	botID int
	page  client.ChatView
	err   error
}

// reconnectDelay is the wait before a dropped changes stream is reopened.
var reconnectDelay = 2 * time.Second

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
		if unauthorized(msg.err) {
			m.client.Token = ""
			return m.openLogin()
		}
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
			return m, m.reconnect()
		}
		return m, next(msg.botID, msg.stream)
	case reconnectMsg:
		if m.chat == nil || m.chat.bot.ID != msg.botID {
			return m, nil
		}
		return m, m.subscribe()
	case changeMsg:
		if m.chat == nil || m.chat.bot.ID != msg.botID {
			if msg.stream != nil {
				_ = msg.stream.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			if msg.stream != nil {
				_ = msg.stream.Close()
			}
			if errors.Is(msg.err, context.Canceled) {
				return m, nil
			}
			if !errors.Is(msg.err, io.EOF) {
				m.err = msg.err
			}
			return m, m.reconnect()
		}
		if msg.change.Type == "snapshot" {
			m.err = nil
		}
		if err := m.chat.view.Apply(msg.change); err != nil {
			m.err = err
		}
		m.chat.loaded = true
		m.chat.syncAnswer()
		m.layout()
		return m, next(msg.botID, msg.stream)
	case sendMsg:
		c := m.chat
		if c == nil || c.bot.ID != msg.botID {
			return m, nil
		}
		m.err = msg.err
		c.retryText, c.retryID = "", ""
		if msg.err != nil {
			if c.input.Value() == "" {
				c.input.SetValue(msg.text)
			}
			if !rejected(msg.err) {
				c.retryText, c.retryID = msg.text, msg.requestID
			}
		}
		return m, nil
	case olderMsg:
		c := m.chat
		if c == nil || c.bot.ID != msg.botID {
			return m, nil
		}
		c.older = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.prepend(msg.page)
		return m, nil
	case answeredMsg:
		return m.answered(msg), nil
	case modelSavedMsg:
		if m.chat != nil && m.chat.bot.ID == msg.botID {
			m.chat.saving = false
		}
		m.err = msg.err
		return m, nil
	case engineMsg:
		return m.showPicker(msg), nil
	case workStreamMsg, workMsg, routineMsg:
		return m.updateWork(msg)
	case pairedMsg:
		return m.paired(msg)
	case loggedOutMsg:
		return m.loggedOut(msg)
	case doneMsg:
		m.err = msg.err
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.closeChat()
			m.closeWork()
			return m, tea.Quit
		}
		if m.screen == screenLogin {
			return m.loginKey(msg)
		}
		if m.screen == screenWork {
			return m.workKey(msg)
		}
		if m.screen == screenChat {
			return m.chatKey(msg)
		}
		return m.botsKey(msg)
	case tea.MouseWheelMsg:
		if m.chat != nil {
			var cmd tea.Cmd
			m.chat.viewport, cmd = m.chat.viewport.Update(msg)
			return m, tea.Batch(cmd, m.loadOlder())
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
	case "L":
		if m.client.Token != "" {
			return m, m.logout()
		}
	case "w":
		if len(m.bots) > 0 {
			return m.openWork(m.bots[m.cursor], nil)
		}
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
	m.chat = &chat{ctx: ctx, bot: bot, cancel: cancel, input: input, viewport: viewport.New()}
	m.screen, m.err = screenChat, nil
	m.layout()
	return m, tea.Batch(m.chat.input.Focus(), m.subscribe())
}

// subscribe opens the changes stream of the open chat; its first event is a full snapshot.
func (m Model) subscribe() tea.Cmd {
	c, ctx, id := m.client, m.chat.ctx, m.chat.bot.ID
	return func() tea.Msg {
		stream, err := c.Subscribe(ctx, "changes", map[string]int{"botId": id})
		return streamMsg{id, stream, err}
	}
}

func (m Model) reconnect() tea.Cmd {
	id := m.chat.bot.ID
	return tea.Tick(reconnectDelay, func(time.Time) tea.Msg { return reconnectMsg{id} })
}

func (m Model) send(text string) tea.Cmd {
	c := m.chat
	id := requestID()
	if text == c.retryText {
		id = c.retryID
	}
	cl, ctx, botID := m.client, m.ctx, c.bot.ID
	return func() tea.Msg {
		err := cl.Call(ctx, "send", map[string]any{"botId": botID, "text": text, "requestId": id}, nil)
		return sendMsg{botID, text, id, err}
	}
}

// rejected reports whether the service answered err, so the request certainly did not apply.
func rejected(err error) bool {
	var e *client.Error
	return errors.As(err, &e) && e.Status < 500
}

// loadOlder reads the page before the oldest loaded message once the transcript is scrolled to the top.
func (m Model) loadOlder() tea.Cmd {
	c := m.chat
	if c == nil || c.older || c.view.Before == "" || !c.viewport.AtTop() {
		return nil
	}
	c.older = true
	cl, ctx, botID, before := m.client, m.ctx, c.bot.ID, c.view.Before
	return func() tea.Msg {
		var page client.ChatView
		err := cl.Call(ctx, "get", map[string]any{"botId": botID, "before": before}, &page)
		return olderMsg{botID, page, err}
	}
}

// prepend adds an older page above the transcript and keeps the lines in view where they were.
func (m *Model) prepend(page client.ChatView) {
	c := m.chat
	known := map[string]bool{}
	for _, msg := range c.view.Messages {
		known[msg.ID] = true
	}
	var older []client.Message
	for _, msg := range page.Messages {
		if !known[msg.ID] {
			older = append(older, msg)
		}
	}
	c.view.Messages = append(older, c.view.Messages...)
	c.view.Before = page.Before
	lines, offset := c.viewport.TotalLineCount(), c.viewport.YOffset()
	c.viewport.SetContent(renderMessages(c.view, c.bot.Name, m.width))
	c.viewport.SetYOffset(offset + c.viewport.TotalLineCount() - lines)
}

func (m *Model) closeChat() {
	if m.chat != nil {
		m.chat.cancel()
		m.chat = nil
	}
}

func (m Model) chatKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c := m.chat
	if c.picker != nil {
		return m.pickerKey(msg)
	}
	if c.answer != nil {
		switch {
		case !c.answer.typing && msg.String() != "esc" && msg.String() != "ctrl+x":
			return m.answerKey(msg)
		case c.answer.typing && msg.String() == "tab":
			c.answer.typing = false
			c.input.Blur()
			return m, nil
		case c.answer.typing && msg.String() == "enter":
			text := strings.TrimSpace(c.input.Value())
			c.input.Reset()
			return m.confirm(text)
		}
	}
	switch msg.String() {
	case "ctrl+l":
		return m, m.openPicker()
	case "ctrl+o":
		c.cancel()
		m.chat = nil
		return m.openWork(c.bot, c)
	case "esc":
		m.closeChat()
		m.screen, m.err = screenBots, nil
		return m, m.loadBots()
	case "enter":
		text := strings.TrimSpace(c.input.Value())
		if c.saving {
			return m, nil
		}
		if text == "" {
			return m, nil
		}
		c.input.Reset()
		return m, m.send(text)
	case "ctrl+x":
		if c.view.Busy {
			return m, m.call("stop", map[string]int{"botId": c.bot.ID})
		}
		return m, nil
	case "pgup":
		c.viewport.PageUp()
		return m, m.loadOlder()
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
	card := 0
	switch {
	case m.chat.picker != nil:
		card = lipgloss.Height(m.pickerView())
	case m.chat.answer != nil:
		card = lipgloss.Height(m.answerView())
	}
	m.chat.viewport.SetHeight(max(m.height-inputHeight-3-card, 1))
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
	titleStyle    = lipgloss.NewStyle().Bold(true)
	faint         = lipgloss.NewStyle().Faint(true)
	selected      = lipgloss.NewStyle().Reverse(true)
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	approvalStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	heading       = lipgloss.NewStyle().Bold(true).Underline(true)
)

// View renders the current screen.
func (m Model) View() tea.View {
	var body string
	switch m.screen {
	case screenLogin:
		body = m.loginView()
	case screenWork:
		body = m.workView()
	case screenChat:
		body = m.chatView()
	default:
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
	b.WriteString("\n" + m.errLine() + faint.Render("↑/↓ select · enter open · w Work · r refresh · L log out · q quit"))
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
	if c.saving {
		header += " · saving model"
	}
	help := "enter send · ctrl+j newline · pgup/pgdn scroll · ctrl+l model · ctrl+o Work · esc Bots"
	if c.view.Busy {
		help = "ctrl+x stop · " + help
	}
	card := ""
	switch {
	case c.picker != nil:
		card = m.pickerView()
		help = "↑/↓ model · ←/→ effort · enter save · esc cancel"
	case c.answer != nil:
		card = m.answerView()
		help = m.answerHelp() + "esc Bots"
	}
	return header + "\n" + c.viewport.View() + "\n" + card + c.input.View() + "\n" + m.errLine() + faint.Render(help)
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

// isEnd reports whether err only means the stream finished or was closed.
func isEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, context.Canceled)
}

// unauthorized reports whether the service refused this device; the hosting guard answers 401 without an oRPC code.
func unauthorized(err error) bool {
	var e *client.Error
	return errors.As(err, &e) && e.Status == 401
}

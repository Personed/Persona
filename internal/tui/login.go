package tui

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// login asks for a pairing code from the service owner and exchanges it for a device token.
type login struct {
	code    textinput.Model
	pairing bool
}

type pairedMsg struct {
	device client.Device
	token  string
	err    error
}

type loggedOutMsg struct{ err error }

func (m Model) openLogin() (tea.Model, tea.Cmd) {
	code := textinput.New()
	code.Placeholder = "pairing code"
	code.Prompt = "> "
	m.login = &login{code: code}
	m.screen = screenLogin
	return m, m.login.code.Focus()
}

func (m Model) loginKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	l := m.login
	switch msg.String() {
	case "esc":
		return m, tea.Quit
	case "enter":
		code := strings.TrimSpace(l.code.Value())
		if code == "" || l.pairing {
			return m, nil
		}
		if !secure(m.client.BaseURL) {
			m.err = errInsecure(m.client.BaseURL)
			return m, nil
		}
		l.pairing, m.err = true, nil
		c, ctx := m.client, m.ctx
		return m, func() tea.Msg {
			device, err := c.Pair(ctx, code, deviceLabel())
			return pairedMsg{device, c.Token, err}
		}
	}
	var cmd tea.Cmd
	l.code, cmd = l.code.Update(msg)
	return m, cmd
}

func (m Model) paired(msg pairedMsg) (tea.Model, tea.Cmd) {
	if m.login != nil {
		m.login.pairing = false
	}
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	if err := m.servers.remember(m.client.BaseURL, msg.token); err != nil {
		m.err = err
	}
	m.login, m.screen = nil, screenBots
	return m, m.loadBots()
}

func (m Model) logout() tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg { return loggedOutMsg{c.Logout(ctx)} }
}

func (m Model) loggedOut(msg loggedOutMsg) (tea.Model, tea.Cmd) {
	m.client.Token, m.bots = "", nil
	if err := m.servers.remember(m.client.BaseURL, ""); err != nil {
		m.err = err
	} else {
		m.err = msg.err
	}
	return m.openLogin()
}

func (m Model) loginView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Persona · Pair this terminal") + "  " + faint.Render(m.client.BaseURL) + "\n\n")
	b.WriteString("Enter a pairing code from the service: create one in the web settings, or use\n")
	b.WriteString("the owner-code file a new install writes to its data directory.\n\n")
	b.WriteString(m.login.code.View() + "\n\n")
	if m.login.pairing {
		b.WriteString(faint.Render("pairing…") + "\n")
	}
	b.WriteString(m.errLine())
	b.WriteString(faint.Render("enter pair · esc quit · --server URL for another service"))
	return b.String()
}

func deviceLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "persona tui"
	}
	return "persona tui on " + host
}

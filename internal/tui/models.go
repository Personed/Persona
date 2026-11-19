package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// picker switches the model and reasoning effort of the open chat's Bot through engines.select,
// the same call the web model setting makes.
type picker struct {
	engine client.Engine
	models []client.EngineModel
	cursor int
	effort map[string]string
}

type engineMsg struct {
	botID  int
	engine client.Engine
	err    error
}

func (m Model) openPicker() tea.Cmd {
	cl, ctx, id := m.client, m.ctx, m.chat.bot.ID
	return func() tea.Msg {
		var e client.Engine
		err := cl.Call(ctx, "engines.get", map[string]int{"botId": id}, &e)
		return engineMsg{id, e, err}
	}
}

func (m Model) showPicker(msg engineMsg) Model {
	if m.chat == nil || m.chat.bot.ID != msg.botID {
		return m
	}
	if msg.err != nil {
		m.err = msg.err
		return m
	}
	p := &picker{engine: msg.engine, models: msg.engine.Models(), effort: map[string]string{}}
	for i, model := range p.models {
		if cfg := msg.engine.Config; cfg.Model != nil && *cfg.Model == model.ID {
			p.cursor = i
			if model.Effort != nil {
				p.effort[model.ID] = cfg.Options[model.Effort.ID]
			}
		}
	}
	if len(p.models) == 0 {
		m.err = fmt.Errorf("%s reports no models", msg.engine.Config.Provider)
		return m
	}
	m.chat.picker = p
	m.layout()
	return m
}

func (m Model) pickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c, p := m.chat, m.chat.picker
	model := p.models[p.cursor]
	switch msg.String() {
	case "esc", "ctrl+l":
		c.picker = nil
		m.layout()
	case "up", "k":
		p.cursor = max(p.cursor-1, 0)
	case "down", "j":
		p.cursor = min(p.cursor+1, len(p.models)-1)
	case "left", "right", "h", "l":
		if model.Effort == nil || len(model.Effort.Options) == 0 {
			return m, nil
		}
		ids := make([]string, len(model.Effort.Options))
		for i, o := range model.Effort.Options {
			ids[i] = o.ID
		}
		i := slices.Index(ids, p.level(model))
		if msg.String() == "left" || msg.String() == "h" {
			i = (i - 1 + len(ids)) % len(ids)
		} else {
			i = (i + 1) % len(ids)
		}
		p.effort[model.ID] = ids[i]
	case "enter":
		options := maps.Clone(p.engine.Config.Options)
		if options == nil {
			options = map[string]string{}
		}
		if model.Effort != nil {
			if level := p.level(model); level != "" {
				options[model.Effort.ID] = level
			} else {
				delete(options, model.Effort.ID)
			}
		}
		c.picker = nil
		m.layout()
		return m, m.call("engines.select", map[string]any{
			"botId": c.bot.ID, "model": model.ID, "options": options,
			"revision": p.engine.Revision, "requestId": requestID(),
		})
	}
	return m, nil
}

// level is the effort chosen for model, defaulting to its first option.
func (p *picker) level(model client.EngineModel) string {
	if level, ok := p.effort[model.ID]; ok && level != "" {
		return level
	}
	if model.Effort != nil && model.Effort.Value != "" {
		return model.Effort.Value
	}
	if model.Effort != nil && len(model.Effort.Options) > 0 {
		return model.Effort.Options[0].ID
	}
	return ""
}

func (m Model) pickerView() string {
	p := m.chat.picker
	var b strings.Builder
	b.WriteString(approvalStyle.Render("Model · "+p.engine.Config.Provider) + "\n")
	for i, model := range p.models {
		line := model.Name
		if model.Name != model.ID {
			line += faint.Render("  " + model.ID)
		}
		if cfg := p.engine.Config; cfg.Model != nil && *cfg.Model == model.ID {
			line += faint.Render("  (current)")
		}
		if model.Effort != nil && len(model.Effort.Options) > 1 {
			line += "  ‹ " + p.level(model) + " ›"
		}
		if i == p.cursor {
			b.WriteString(selected.Render("> "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

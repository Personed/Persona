package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"

	"github.com/Personed/Persona/internal/store"
)

// BotNameLength is BOT_NAME_LENGTH.
const BotNameLength = 80

// Bot is a directory entry as the API returns it.
type Bot struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar"`
	CreatedAt *int64 `json:"createdAt,omitempty"`
}

// BotPresence is a Bot with its chat state, an item of bots.list.
type BotPresence struct {
	Bot
	WelcomeComplete bool     `json:"welcomeComplete"`
	Busy            bool     `json:"busy"`
	Preview         string   `json:"preview"`
	LastMessageAt   *float64 `json:"lastMessageAt,omitempty"`
}

type directoryItem struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	Avatar          string  `json:"avatar"`
	RequestID       *string `json:"requestId"`
	CreatedAt       *int64  `json:"createdAt,omitempty"`
	WelcomeComplete bool    `json:"welcomeComplete,omitempty"`
	Revision        int     `json:"revision,omitempty"`
	UpdateID        string  `json:"updateId,omitempty"`
}

type directory struct {
	Items []directoryItem `json:"items"`
}

// Visibility hides one model of a Harness from model pickers.
type Visibility struct {
	Harness string `json:"harness"`
	Model   string `json:"model"`
	Visible bool   `json:"visible"`
}

type visibilityDoc struct {
	Items []Visibility `json:"items"`
}

// BotDirectory and ModelVisibility are the session documents of packages/agent/src/bots.ts.
var (
	BotDirectory    = Doc[directory]{Kind: "persona.bots", Scope: "session", Initial: func() directory { return directory{Items: []directoryItem{}} }}
	ModelVisibility = Doc[visibilityDoc]{Kind: "persona.model-visibility", Scope: "session", Initial: func() visibilityDoc { return visibilityDoc{Items: []Visibility{}} }}
)

// CreateBot is the input of Bots.Create.
type CreateBot struct {
	Name         string    `json:"name"`
	Instructions string    `json:"instructions"`
	Model        *ModelRef `json:"model,omitempty"`
	RequestID    string    `json:"requestId"`
	Avatar       string    `json:"avatar,omitempty"`
}

// UpdateBot is the input of Bots.Update.
type UpdateBot struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	Avatar       string `json:"avatar"`
	Revision     int    `json:"revision"`
	RequestID    string `json:"requestId"`
}

// Profile is a Bot's editable identity.
type Profile struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Avatar       string    `json:"avatar"`
	Revision     int       `json:"revision"`
	Instructions string    `json:"instructions"`
	Model        *ModelRef `json:"model,omitempty"`
}

// Bots is the Bot directory. A Bot's ID is its conversation ID.
type Bots struct {
	h   *Harness
	now func() time.Time
}

// NewBots returns the directory over h. now defaults to time.Now.
func NewBots(h *Harness, now func() time.Time) *Bots {
	if now == nil {
		now = time.Now
	}
	return &Bots{h: h, now: now}
}

func (b *Bots) millis() *int64 {
	ms := b.now().UnixMilli()
	return &ms
}

// Initialize adds the root conversation as the Bot "Persona" and gives every Bot an avatar.
func (b *Bots) Initialize(ctx context.Context, conversation store.ConversationID) error {
	return b.h.Commit(ctx, func(tx *Tx) error {
		dir, err := Edit(tx, BotDirectory, 0)
		if err != nil {
			return err
		}
		if dir.find(int64(conversation)) == nil {
			dir.Items = append(dir.Items, directoryItem{ID: int64(conversation), Name: "Persona", Avatar: newUUID(), CreatedAt: b.millis()})
		}
		for i := range dir.Items {
			if dir.Items[i].Avatar == "" {
				dir.Items[i].Avatar = newUUID()
			}
		}
		return nil
	})
}

func (d *directory) find(id int64) *directoryItem {
	for i := range d.Items {
		if d.Items[i].ID == id {
			return &d.Items[i]
		}
	}
	return nil
}

// ModelVisibility lists the hidden models.
func (b *Bots) ModelVisibility(ctx context.Context) ([]Visibility, error) {
	doc, _, err := Snapshot(ctx, b.h, ModelVisibility, 0)
	if doc.Items == nil {
		doc.Items = []Visibility{}
	}
	return doc.Items, err
}

// SetModelVisibility shows or hides one model and returns the hidden models.
func (b *Bots) SetModelVisibility(ctx context.Context, input Visibility) ([]Visibility, error) {
	err := b.h.Commit(ctx, func(tx *Tx) error {
		doc, err := Edit(tx, ModelVisibility, 0)
		if err != nil {
			return err
		}
		items := []Visibility{}
		for _, item := range doc.Items {
			if item.Harness != input.Harness || item.Model != input.Model {
				items = append(items, item)
			}
		}
		if !input.Visible {
			items = append(items, input)
		}
		doc.Items = items
		return nil
	})
	if err != nil {
		return nil, err
	}
	return b.ModelVisibility(ctx)
}

// List returns every Bot with its busy state and chat preview.
func (b *Bots) List(ctx context.Context) ([]BotPresence, error) {
	dir, _, err := Snapshot(ctx, b.h, BotDirectory, 0)
	if err != nil {
		return nil, err
	}
	out := make([]BotPresence, 0, len(dir.Items))
	for _, item := range dir.Items {
		presence := BotPresence{
			Bot:             Bot{ID: item.ID, Name: item.Name, Avatar: item.Avatar, CreatedAt: item.CreatedAt},
			WelcomeComplete: item.WelcomeComplete,
		}
		live, _, err := Snapshot(ctx, b.h, LiveDoc, item.ID)
		if err != nil {
			return nil, err
		}
		engine, _, err := Snapshot(ctx, b.h, EngineDoc, item.ID)
		if err != nil {
			return nil, err
		}
		presence.Busy = live.running() || len(engine.Tasks) > 0
		if _, err := b.h.store.Conversation(ctx, store.ConversationID(item.ID)); err == nil {
			agent, _, err := Snapshot(ctx, b.h, AgentDoc, item.ID)
			if err != nil {
				return nil, err
			}
			if presence.Preview, presence.LastMessageAt, err = botPreview(ctx, b.h.store, item.ID, agent.Instructions()); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		out = append(out, presence)
	}
	return out, nil
}

// Exists reports whether id is a Bot with a conversation, as bots.conversation does.
func (b *Bots) Exists(ctx context.Context, id int64) (bool, error) {
	dir, _, err := Snapshot(ctx, b.h, BotDirectory, 0)
	if err != nil || dir.find(id) == nil {
		return false, err
	}
	_, err = b.h.store.Conversation(ctx, store.ConversationID(id))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CompleteWelcome marks the Bot's welcome as shown.
func (b *Bots) CompleteWelcome(ctx context.Context, id int64) error {
	return b.h.Commit(ctx, func(tx *Tx) error {
		dir, err := Edit(tx, BotDirectory, 0)
		if err != nil {
			return err
		}
		bot := dir.find(id)
		if bot == nil {
			return ErrNoBot
		}
		bot.WelcomeComplete = true
		return nil
	})
}

// Profile returns the Bot's identity and configuration.
func (b *Bots) Profile(ctx context.Context, id int64) (Profile, error) {
	dir, _, err := Snapshot(ctx, b.h, BotDirectory, 0)
	if err != nil {
		return Profile{}, err
	}
	bot := dir.find(id)
	if bot == nil {
		return Profile{}, ErrNoBot
	}
	if ok, err := b.Exists(ctx, id); err != nil || !ok {
		if err == nil {
			err = ErrNoBot
		}
		return Profile{}, err
	}
	agent, _, err := Snapshot(ctx, b.h, AgentDoc, id)
	if err != nil {
		return Profile{}, err
	}
	return Profile{ID: bot.ID, Name: bot.Name, Avatar: bot.Avatar, Revision: bot.Revision,
		Instructions: agent.Instructions(), Model: agent.Model()}, nil
}

// Update changes the Bot's identity and instructions at the expected revision.
// A repeated request ID is a no-op.
func (b *Bots) Update(ctx context.Context, id int64, input UpdateBot) (Profile, error) {
	err := b.h.Commit(ctx, func(tx *Tx) error {
		dir, err := Edit(tx, BotDirectory, 0)
		if err != nil {
			return err
		}
		bot := dir.find(id)
		if bot == nil {
			return ErrNoBot
		}
		if bot.UpdateID == input.RequestID {
			return nil
		}
		if bot.Revision != input.Revision {
			return &WorkError{Code: "CONFLICT", Message: "内容已更新，请刷新后重试。"}
		}
		name := trimJS(input.Name)
		if n := len(utf16.Encode([]rune(name))); n == 0 || n > BotNameLength {
			return &WorkError{Code: "BAD_REQUEST", Message: fmt.Sprintf("内容长度须为1至%d个字符。", BotNameLength)}
		}
		bot.Name, bot.Avatar, bot.Revision, bot.UpdateID = name, input.Avatar, bot.Revision+1, input.RequestID
		agent, err := Edit(tx, AgentDoc, id)
		if err != nil {
			return err
		}
		agent.apply(Change{Instructions: &input.Instructions})
		return nil
	})
	if err != nil {
		return Profile{}, err
	}
	return b.Profile(ctx, id)
}

// Create adds a Bot with its own conversation. A repeated request ID returns the Bot it created.
func (b *Bots) Create(ctx context.Context, input CreateBot) (Bot, error) {
	var bot Bot
	err := b.h.Commit(ctx, func(tx *Tx) error {
		dir, err := Edit(tx, BotDirectory, 0)
		if err != nil {
			return err
		}
		for _, item := range dir.Items {
			if item.RequestID != nil && *item.RequestID == input.RequestID {
				bot = Bot{ID: item.ID, Name: item.Name, Avatar: item.Avatar, CreatedAt: item.CreatedAt}
				return nil
			}
		}
		conversation, err := tx.CreateConversation()
		if err != nil {
			return err
		}
		agent, err := Edit(tx, AgentDoc, int64(conversation))
		if err != nil {
			return err
		}
		agent.apply(Change{Model: input.Model, Instructions: &input.Instructions})
		bot = Bot{ID: int64(conversation), Name: input.Name, Avatar: input.Avatar, CreatedAt: b.millis()}
		if bot.Avatar == "" {
			bot.Avatar = newUUID()
		}
		requestID := input.RequestID
		dir.Items = append(dir.Items, directoryItem{ID: bot.ID, Name: bot.Name, Avatar: bot.Avatar, CreatedAt: bot.CreatedAt, RequestID: &requestID})
		return nil
	})
	return bot, err
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

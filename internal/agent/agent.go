package agent

import (
	"context"
	"time"

	"github.com/Personed/Persona/internal/store"
)

// Options configure Open.
type Options struct {
	Models    Models
	FilesRoot string
	// Agent configures the root conversation when Open creates it.
	Agent Change
	Now   func() time.Time
}

// Agent is the opened domain, the result of openAgent in open-agent.ts.
type Agent struct {
	Harness *Harness
	Bots    *Bots
	Files   *Files
	Models  Models
	Root    store.ConversationID
}

// Open composes the Bot domain over records: it creates the root conversation
// on first use and adds it to the Bot directory as "Persona".
func Open(ctx context.Context, records store.Store, opts Options) (*Agent, error) {
	h := NewHarness(records)
	a := &Agent{Harness: h, Bots: NewBots(h, opts.Now), Files: NewFiles(opts.FilesRoot), Models: opts.Models}
	page, err := records.Conversations(ctx, store.ConversationQuery{}, store.Scan{Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(page.Items) > 0 {
		a.Root = page.Items[0].ID
	} else if err := h.Commit(ctx, func(tx *Tx) error {
		if a.Root, err = tx.CreateConversation(); err != nil {
			return err
		}
		agent, err := Edit(tx, AgentDoc, int64(a.Root))
		if err != nil {
			return err
		}
		agent.apply(opts.Agent)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := a.Bots.Initialize(ctx, a.Root); err != nil {
		return nil, err
	}
	return a, nil
}

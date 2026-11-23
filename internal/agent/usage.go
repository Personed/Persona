package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/Personed/Persona/internal/store"
)

// UsageFilter selects usage records, UsageFilter in harness/usage.ts.
type UsageFilter struct {
	BotID    *int64   `json:"botId"`
	Harness  string   `json:"harness"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	From     *float64 `json:"from"`
	To       *float64 `json:"to"`
}

// UsageRecord is one usage row of engines.usage: a stored persona.engine-usage
// entry as written, or one derived from a pi.assistant entry.
type UsageRecord map[string]any

func (u UsageRecord) text(key string) string {
	s, _ := u[key].(string)
	return s
}

func (u UsageRecord) timestamp() float64 {
	n, _ := u["timestamp"].(float64)
	return n
}

// Usage reads the usage records of bots, newest first, readUsage in harness/usage.ts.
func (a *Agent) Usage(ctx context.Context, bots []int64, filter UsageFilter) ([]UsageRecord, error) {
	rows := []UsageRecord{}
	for _, id := range bots {
		if filter.BotID != nil && id != *filter.BotID {
			continue
		}
		if _, err := a.Harness.store.Conversation(ctx, store.ConversationID(id)); errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		scan := store.Scan{Limit: 500, Descending: true}
		for {
			page, err := a.Harness.store.Entries(ctx, store.EntryQuery{ConversationID: store.ConversationID(id)}, scan)
			if err != nil {
				return nil, err
			}
			for _, entry := range page.Items {
				row, ok := usageRow(id, entry)
				if ok && filter.match(row) {
					rows = append(rows, row)
				}
			}
			if page.Next == "" {
				break
			}
			scan.Cursor = page.Next
		}
	}
	slices.SortStableFunc(rows, func(x, y UsageRecord) int {
		return cmp.Or(cmp.Compare(y.timestamp(), x.timestamp()), cmp.Compare(x.text("taskId"), y.text("taskId")),
			cmp.Compare(x.text("id"), y.text("id")))
	})
	return rows, nil
}

func (f UsageFilter) match(row UsageRecord) bool {
	switch {
	case f.Harness != "" && row.text("harness") != f.Harness,
		f.Provider != "" && row.text("provider") != f.Provider,
		f.Model != "" && row.text("model") != f.Model,
		f.From != nil && row.timestamp() < *f.From,
		f.To != nil && row.timestamp() > *f.To:
		return false
	}
	return true
}

func usageRow(bot int64, entry store.Entry) (UsageRecord, bool) {
	switch entry.Kind {
	case engineUsageEntry:
		var row UsageRecord
		if json.Unmarshal(entry.Data, &row) != nil || row == nil {
			row = UsageRecord{}
		}
		return row, true
	case "pi.assistant":
	default:
		return nil, false
	}
	var messages []struct {
		Role      string  `json:"role"`
		Provider  *string `json:"provider"`
		Model     *string `json:"model"`
		Timestamp float64 `json:"timestamp"`
		Usage     *struct {
			Input      *float64 `json:"input"`
			Output     *float64 `json:"output"`
			CacheRead  *float64 `json:"cacheRead"`
			CacheWrite *float64 `json:"cacheWrite"`
			Cost       *struct {
				Total *float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(entry.Model, &messages)
	taskID := int64(entry.ID)
	if entry.ByTaskID != nil {
		taskID = int64(*entry.ByTaskID)
	}
	row := UsageRecord{"id": strconv.FormatInt(int64(entry.ID), 10), "source": "pi-durable", "provider": nil, "model": nil,
		"input": nil, "output": nil, "cacheRead": nil, "cacheWrite": nil, "costUsd": nil, "botId": bot, "harness": "pi",
		"taskId": strconv.FormatInt(taskID, 10), "sessionId": nil, "timestamp": float64(0)}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		if m.Provider != nil {
			row["provider"] = *m.Provider
		}
		if m.Model != nil {
			row["model"] = *m.Model
		}
		row["timestamp"] = m.Timestamp
		if u := m.Usage; u != nil {
			for key, value := range map[string]*float64{"input": u.Input, "output": u.Output, "cacheRead": u.CacheRead, "cacheWrite": u.CacheWrite} {
				if value != nil {
					row[key] = *value
				}
			}
			if u.Cost != nil && u.Cost.Total != nil {
				row["costUsd"] = *u.Cost.Total
			}
		}
		break
	}
	return row, true
}

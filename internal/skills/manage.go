package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/Personed/Persona/internal/store"
)

// Export is a skill package as work.skills.export returns it: the base64 ZIP.
type Export struct {
	Name    string `json:"name"`
	Archive string `json:"archive"`
}

var sessionScope = store.Scope{Kind: "session"}

// List returns every managed skill, as Skills.list in packages/agent/src/work/skills.ts.
func (s *Service) List(ctx context.Context) ([]Skill, error) {
	items, err := s.list(ctx)
	if items == nil {
		items = []Skill{}
	}
	return items, err
}

// change applies edit to the skills document under mu and writes it when edit reports a change.
func (s *Service) change(ctx context.Context, edit func(items []Skill) ([]Skill, bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := s.store.FindDocument(ctx, DocKind, sessionScope, "", store.Current)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var doc struct {
		Items []Skill `json:"items"`
	}
	if err == nil {
		value, err := s.store.Document(ctx, meta.ID, store.Current)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(value.Value, &doc); err != nil {
			return err
		}
	}
	items, changed, err := edit(doc.Items)
	if err != nil || !changed {
		return err
	}
	if items == nil {
		items = []Skill{}
	}
	doc.Items = items
	value, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	write := store.Write{Type: store.WriteDocumentChange, Document: &meta, Value: value}
	if meta.ID == 0 {
		id, err := s.store.MintID(ctx)
		if err != nil {
			return err
		}
		meta = store.Document{ID: store.DocumentID(id), Kind: DocKind, Scope: sessionScope}
		write = store.Write{Type: store.WriteDocumentCreate, Document: &meta, Value: value}
	}
	_, _, err = s.store.Commit(ctx, []store.Batch{{write}})
	return err
}

func requireRevision(actual, expected int) error {
	if actual != expected {
		return &Error{Conflict, "内容已更新，请刷新后重试。"}
	}
	return nil
}

// Enable turns a skill on or off for one Bot at its current revision. A
// repeated requestID changes nothing.
func (s *Service) Enable(ctx context.Context, botID int64, id string, enabled bool, revision int, requestID string) error {
	return s.change(ctx, func(items []Skill) ([]Skill, bool, error) {
		index := slices.IndexFunc(items, func(skill Skill) bool { return skill.ID == id })
		if index < 0 {
			return nil, false, &Error{NotFound, "技能不存在。"}
		}
		skill := &items[index]
		if skill.RequestID == requestID {
			return nil, false, nil
		}
		if err := requireRevision(skill.Revision, revision); err != nil {
			return nil, false, err
		}
		bots := slices.DeleteFunc(slices.Clone(skill.EnabledBots), func(bot int64) bool { return bot == botID })
		if bots == nil {
			bots = []int64{}
		}
		if enabled {
			bots = append(bots, botID)
		}
		skill.EnabledBots = bots
		skill.Revision++
		skill.RequestID = requestID
		return items, true, nil
	})
}

// Delete removes a skill at its current revision. A missing skill is not an error.
func (s *Service) Delete(ctx context.Context, id string, revision int) error {
	return s.change(ctx, func(items []Skill) ([]Skill, bool, error) {
		index := slices.IndexFunc(items, func(skill Skill) bool { return skill.ID == id })
		if index < 0 {
			return nil, false, nil
		}
		if err := requireRevision(items[index].Revision, revision); err != nil {
			return nil, false, err
		}
		return slices.Delete(items, index, index+1), true, nil
	})
}

// Export returns a skill's current package, or the saved version hash names.
// A skill saved before packages existed exports its SKILL.md alone.
func (s *Service) Export(ctx context.Context, id, hash string) (Export, error) {
	items, err := s.list(ctx)
	if err != nil {
		return Export{}, err
	}
	index := slices.IndexFunc(items, func(skill Skill) bool { return skill.ID == id })
	if index < 0 || (hash != "" && !slices.ContainsFunc(items[index].Versions, func(v Version) bool { return v.Hash == hash })) {
		return Export{}, &Error{NotFound, "技能版本不存在。"}
	}
	skill := items[index]
	if s.packages == nil {
		return Export{}, &Error{BadRequest, "技能包存储不可用。"}
	}
	if hash == "" {
		hash = skill.Hash
	}
	if hash == "" {
		var archive bytes.Buffer
		w := zip.NewWriter(&archive)
		f, err := w.Create("SKILL.md")
		if err == nil {
			_, err = f.Write([]byte(skill.Content))
		}
		if err == nil {
			err = w.Close()
		}
		if err != nil {
			return Export{}, err
		}
		return Export{Name: skill.Name, Archive: base64.StdEncoding.EncodeToString(archive.Bytes())}, nil
	}
	if !hashPattern.MatchString(hash) {
		return Export{}, &Error{BadRequest, "版本哈希无效。"}
	}
	file, err := s.packages.Open(hash + ".zip")
	if err != nil {
		return Export{}, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return Export{}, err
	}
	return Export{Name: skill.Name, Archive: base64.StdEncoding.EncodeToString(data)}, nil
}

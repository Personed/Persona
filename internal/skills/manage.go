package skills

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	if s.packages == nil && s.packageDir == "" {
		return Export{}, &Error{BadRequest, "技能包存储不可用。"}
	}
	if hash == "" {
		hash = skill.Hash
	}
	if hash == "" {
		// A skill saved before packages existed exports its SKILL.md alone,
		// stored under its own hash (skills.ts export).
		bundle, err := packageBundle(skill.Content, "")
		if err != nil {
			return Export{}, err
		}
		if err := s.storePackage(bundle); err != nil {
			return Export{}, err
		}
		hash = bundle.Hash
	}
	if !hashPattern.MatchString(hash) {
		return Export{}, &Error{BadRequest, "版本哈希无效。"}
	}
	data, err := s.readPackage(hash)
	if err != nil {
		return Export{}, err
	}
	return Export{Name: skill.Name, Archive: base64.StdEncoding.EncodeToString(data)}, nil
}

// SaveInput is a work.skills.save call, as SkillInput in skills.ts.
type SaveInput struct {
	ID        string
	Content   string
	Archive   string
	Revision  int
	RequestID string
}

// Save upserts a skill at its current revision, parsing its SKILL.md and
// storing the package version, as saveSkill in skills.ts.
func (s *Service) Save(ctx context.Context, input SaveInput) (Skill, error) {
	var saved Skill
	err := s.change(ctx, func(items []Skill) ([]Skill, bool, error) {
		index := slices.IndexFunc(items, func(skill Skill) bool { return skill.ID == input.ID })
		var previous *Skill
		if index >= 0 {
			previous = &items[index]
		}
		if previous != nil && previous.RequestID == input.RequestID {
			saved = *previous
			return items, false, nil
		}
		revision := 0
		if previous != nil {
			revision = previous.Revision
		}
		if err := requireRevision(revision, input.Revision); err != nil {
			return nil, false, err
		}
		archive := input.Archive
		if archive == "" && previous != nil && previous.Hash != "" && (s.packages != nil || s.packageDir != "") {
			data, err := s.readPackage(previous.Hash)
			if err == nil {
				archive = base64.StdEncoding.EncodeToString(data)
			}
		}
		bundle, err := packageBundle(input.Content, archive)
		if err != nil {
			return nil, false, err
		}
		if slices.ContainsFunc(items, func(skill Skill) bool {
			return skill.Name == bundle.Name && skill.ID != input.ID
		}) {
			return nil, false, &Error{Conflict, "技能名已存在。"}
		}
		if err := s.storePackage(bundle); err != nil {
			return nil, false, err
		}
		var versions []Version
		if previous != nil {
			versions = slices.Clone(previous.Versions)
			if versions == nil {
				prior, err := packageBundle(previous.Content, "")
				if err == nil {
					versions = []Version{{Hash: prior.Hash, Version: 1}}
					if previous.Hash == "" {
						if err := s.storePackage(prior); err != nil {
							return nil, false, err
						}
					}
				}
			}
		}
		if len(versions) == 0 || versions[len(versions)-1].Hash != bundle.Hash {
			last := 0
			if len(versions) > 0 {
				last = versions[len(versions)-1].Version
			}
			versions = append(versions, Version{Hash: bundle.Hash, Version: last + 1})
		}
		skill := Skill{
			ID: input.ID, Name: bundle.Name, Description: bundle.Description,
			Content: bundle.Content, Hash: bundle.Hash, Versions: versions,
			Revision: input.Revision + 1,
			Source:   "user", RequestID: input.RequestID,
		}
		if previous != nil {
			skill.EnabledBots = previous.EnabledBots
			*previous = skill
		} else {
			skill.EnabledBots = []int64{}
			items = append(items, skill)
		}
		saved = skill
		return items, true, nil
	})
	return saved, err
}

// PreviewChange is one file difference work.skills.preview returns.
type PreviewChange struct {
	Path   string  `json:"path"`
	Status string  `json:"status"`
	Before *string `json:"before"`
	After  *string `json:"after"`
}

// Preview parses a candidate SKILL.md + archive and diffs its files against
// the saved version, as Skills.preview in skills.ts.
func (s *Service) Preview(ctx context.Context, id, content, archive string) (any, error) {
	items, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	var previous *Skill
	if index := slices.IndexFunc(items, func(skill Skill) bool { return skill.ID == id }); index >= 0 {
		previous = &items[index]
	}
	if archive == "" && previous != nil && previous.Hash != "" && (s.packages != nil || s.packageDir != "") {
		data, err := s.readPackage(previous.Hash)
		if err == nil {
			archive = base64.StdEncoding.EncodeToString(data)
		}
	}
	bundle, err := packageBundle(content, archive)
	if err != nil {
		return nil, err
	}
	var before *Bundle
	if previous != nil {
		prevArchive := ""
		if previous.Hash != "" && (s.packages != nil || s.packageDir != "") {
			if data, err := s.readPackage(previous.Hash); err == nil {
				prevArchive = base64.StdEncoding.EncodeToString(data)
			}
		}
		if b, err := packageBundle(previous.Content, prevArchive); err == nil {
			before = &b
		}
	}
	seen := map[string]bool{}
	var names []string
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			names = append(names, path)
		}
	}
	if before != nil {
		for _, path := range before.Paths {
			add(path)
		}
	}
	for _, name := range bundle.Paths {
		add(name)
	}
	slices.Sort(names)
	changes := []PreviewChange{}
	for _, name := range names {
		var old, next []byte
		if before != nil {
			old = before.Files[name]
		}
		next = bundle.Files[name]
		if old != nil && next != nil && bytes.Equal(old, next) {
			continue
		}
		status := "changed"
		if old == nil {
			status = "added"
		} else if next == nil {
			status = "deleted"
		}
		changes = append(changes, PreviewChange{
			Path: name, Status: status,
			Before: textOf(old), After: textOf(next),
		})
	}
	return map[string]any{
		"content": bundle.Content, "hash": bundle.Hash,
		"name": bundle.Name, "description": bundle.Description,
		"changes": changes,
	}, nil
}

// Restore saves the package a version hash names back over the skill, as
// Skills.restore in skills.ts.
func (s *Service) Restore(ctx context.Context, id, hash string, revision int, requestID string) (Skill, error) {
	export, err := s.Export(ctx, id, hash)
	if err != nil {
		return Skill{}, err
	}
	return s.Save(ctx, SaveInput{
		ID: id, Content: "", Archive: export.Archive,
		Revision: revision, RequestID: requestID,
	})
}

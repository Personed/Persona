package routines

import (
	"cmp"
	"context"
	"slices"

	"github.com/Personed/Persona/internal/store"
)

// Input is a routine the user saves in the work panel, as RoutineInput in
// packages/agent/src/work/routine-state.ts. Nil fields keep the stored value.
type Input struct {
	ID           string   `json:"id"`
	Revision     int      `json:"revision"`
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Schedule     Schedule `json:"schedule"`
	SkillIDs     []string `json:"skillIds"`
	Delivery     string   `json:"delivery"`
	Condition    *string  `json:"condition"`
	ExecutionMs  *int64   `json:"executionMs"`
	RequestID    string   `json:"requestId"`
}

// Save stores a user's routine disabled, as Routines.save in routines.ts;
// enabling it is a separate request. A repeated requestID returns the stored
// routine unchanged.
func (s *Service) Save(ctx context.Context, botID int64, input Input) (Routine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var doc document
	meta, err := s.read(ctx, DocKind, &doc)
	if err != nil {
		return Routine{}, err
	}
	index := slices.IndexFunc(doc.Items, func(item Routine) bool { return item.ID == input.ID })
	previous := Routine{}
	if index >= 0 {
		previous = doc.Items[index]
		if previous.BotID != botID {
			return Routine{}, &Error{NotFound, "定时任务不存在。"}
		}
		if previous.RequestID == input.RequestID {
			return previous, nil
		}
	}
	if previous.Revision != input.Revision {
		return Routine{}, &Error{Conflict, "内容已更新，请刷新后重试。"}
	}
	if len(input.SkillIDs) > 0 {
		var skills struct {
			Items []skill `json:"items"`
		}
		if _, err := s.read(ctx, skillsDocKind, &skills); err != nil {
			return Routine{}, err
		}
		for _, id := range input.SkillIDs {
			if !slices.ContainsFunc(skills.Items, func(skill skill) bool {
				return skill.ID == id && slices.Contains(skill.EnabledBots, botID)
			}) {
				return Routine{}, &Error{NotFound, "技能已删除或未为此Bot启用。"}
			}
		}
	}
	if input.ExecutionMs != nil && (*input.ExecutionMs < MinExecutionMs || *input.ExecutionMs > MaxExecutionMs) {
		return Routine{}, &Error{BadRequest, "执行期限必须在 1 秒至 24 小时之间。"}
	}
	item := previous
	item.Delivery = cmp.Or(input.Delivery, previous.Delivery, Always)
	item.Condition = ""
	if item.Delivery == Conditional {
		condition := previous.Condition
		if input.Condition != nil {
			condition = *input.Condition
		}
		if item.Condition, err = requiredText(condition, TextLength); err != nil {
			return Routine{}, err
		}
	}
	item.ExecutionMs = previous.ExecutionMs
	if input.ExecutionMs != nil {
		item.ExecutionMs = *input.ExecutionMs
	}
	item.ExecutionMs = cmp.Or(item.ExecutionMs, DefaultExecutionMs)
	if item.Title, err = requiredText(input.Title, TitleLength); err != nil {
		return Routine{}, err
	}
	if item.Instructions, err = requiredText(input.Instructions, TextLength); err != nil {
		return Routine{}, err
	}
	if err := input.Schedule.Validate(s.now()); err != nil {
		return Routine{}, err
	}
	if index < 0 || item.Delivery != previous.Delivery || item.Condition != previous.Condition {
		item.Baseline, item.NotifiedKey = "", ""
	}
	item.ID, item.BotID, item.Schedule, item.SkillIDs = input.ID, botID, input.Schedule, unique(input.SkillIDs)
	item.Revision, item.RequestID, item.Source = input.Revision+1, input.RequestID, "user"
	item.Enabled, item.NextAt, item.TaskID = false, nil, nil
	if index < 0 {
		doc.Items = append(doc.Items, item)
	} else {
		doc.Items[index] = item
	}
	return item, s.save(ctx, meta, doc)
}

// Enable turns a routine on or off at its current revision and schedules its
// next run, as Routines.enable in routines.ts.
func (s *Service) Enable(ctx context.Context, botID int64, id string, enabled bool, revision int, requestID string) error {
	action := "pause"
	if enabled {
		action = "resume"
	}
	_, _, err := s.apply(ctx, botID, Change{Action: action, ID: id, Revision: &revision}, requestID, "user", false)
	return err
}

// Delete removes a routine at its current revision. A missing routine is not an error.
func (s *Service) Delete(ctx context.Context, botID int64, id string, revision int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var doc document
	meta, err := s.read(ctx, DocKind, &doc)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(doc.Items, func(item Routine) bool { return item.BotID == botID && item.ID == id })
	if index < 0 {
		return nil
	}
	if doc.Items[index].Revision != revision {
		return &Error{Conflict, "内容已更新，请刷新后重试。"}
	}
	doc.Items = slices.Delete(doc.Items, index, index+1)
	return s.save(ctx, meta, doc)
}

// Test starts one test run of a routine through RunNow, as Routines.test in routines.ts.
func (s *Service) Test(ctx context.Context, botID int64, id, requestID string) (store.TaskID, error) {
	var doc document
	if _, err := s.read(ctx, DocKind, &doc); err != nil {
		return 0, err
	}
	index := slices.IndexFunc(doc.Items, func(item Routine) bool { return item.BotID == botID && item.ID == id })
	if index < 0 {
		return 0, &Error{NotFound, "定时任务不存在。"}
	}
	if s.RunNow == nil {
		return 0, &Error{BadRequest, "定时执行服务不可用。"}
	}
	return s.RunNow(ctx, doc.Items[index], requestID)
}

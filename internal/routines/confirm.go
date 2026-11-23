package routines

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/approval"
)

var (
	actionLabels = map[string]string{
		"create": "创建", "update": "修改", "pause": "暂停",
		"resume": "恢复", "run_now": "立即执行", "delete": "删除",
	}
	weekdays          = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	expressionPattern = regexp.MustCompile(`^(\d{1,2}) (\d{1,2}) \* \* (\*|1-5|[0-6])$`)
)

// confirm asks the user to approve a Bot's routine change on the routine card,
// as Routines.manage in packages/agent/src/work/routines.ts does. A repeated
// request ID needs no answer, since Manage returns the stored routine.
func (s *Service) confirm(ctx context.Context, botID int64, change Change, requestID string) (bool, error) {
	if s.Confirm == nil {
		return false, &Error{BadRequest, "确认服务尚未启动。"}
	}
	var doc document
	if _, err := s.read(ctx, DocKind, &doc); err != nil {
		return false, err
	}
	index := slices.IndexFunc(doc.Items, func(item Routine) bool {
		return item.BotID == botID && (item.RequestID == requestID || change.Action != "create" && item.ID == change.ID)
	})
	var current *Routine
	if index >= 0 {
		if doc.Items[index].RequestID == requestID {
			return true, nil
		}
		current = &doc.Items[index]
	}
	if change.Action != "create" && current == nil {
		return false, &Error{NotFound, "定时任务不存在。"}
	}
	if current != nil && (change.Revision == nil || *change.Revision != current.Revision) {
		return false, &Error{Conflict, "内容已更新，请刷新后重试。"}
	}
	input := Routine{BotID: botID}
	if current != nil {
		input = *current
	}
	if change.Title != nil {
		input.Title = *change.Title
	}
	if change.Instructions != nil {
		input.Instructions = *change.Instructions
	}
	if change.Schedule != nil {
		input.Schedule = *change.Schedule
	}
	if change.SkillIDs != nil {
		input.SkillIDs = change.SkillIDs
	}
	if change.Delivery != "" {
		input.Delivery = change.Delivery
	}
	if change.Condition != nil {
		input.Condition = *change.Condition
	}
	if change.ExecutionMs != nil {
		input.ExecutionMs = *change.ExecutionMs
	}
	if change.Action == "create" || change.Action == "update" {
		if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Instructions) == "" || input.Schedule.Type == "" {
			return false, &Error{BadRequest, "缺少名称、要求或执行时间。"}
		}
		if err := input.Schedule.Validate(s.now()); err != nil {
			return false, err
		}
	}
	fields := routineFields(input)
	var previous []approval.Field
	if current != nil {
		previous = routineFields(*current)
	}
	if change.Action == "update" && input.Title != current.Title {
		fields = append([]approval.Field{{Label: "任务名称", Value: input.Title}}, fields...)
		previous = append([]approval.Field{{Label: "任务名称", Value: current.Title}}, previous...)
	}
	if len(input.SkillIDs) > 0 || current != nil && len(current.SkillIDs) > 0 {
		var skills struct {
			Items []skill `json:"items"`
		}
		if _, err := s.read(ctx, skillsDocKind, &skills); err != nil {
			return false, err
		}
		names := func(ids []string) string {
			labels := make([]string, len(ids))
			for i, id := range ids {
				labels[i] = id
				if at := slices.IndexFunc(skills.Items, func(item skill) bool { return item.ID == id }); at >= 0 {
					labels[i] = skills.Items[at].Name
				}
			}
			return cmp.Or(strings.Join(labels, "、"), "未绑定")
		}
		fields = append(fields, approval.Field{Label: "使用技能", Value: names(input.SkillIDs)})
		var before []string
		if current != nil {
			before = current.SkillIDs
		}
		previous = append(previous, approval.Field{Label: "使用技能", Value: names(before)})
	}
	if change.Action == "update" {
		changed := fields[:0]
		for _, field := range fields {
			at := slices.IndexFunc(previous, func(item approval.Field) bool { return item.Label == field.Label })
			switch {
			case at < 0:
				changed = append(changed, field)
			case previous[at].Value != field.Value:
				changed = append(changed, approval.Field{Label: field.Label, Value: previous[at].Value + " → " + field.Value})
			}
		}
		fields = changed
	}
	detail := input.Instructions
	if change.Action == "update" && input.Instructions == current.Instructions {
		detail = ""
	}
	answer, err := s.Confirm(ctx, botID, approval.Request{
		ID: "routine:" + requestID, Title: actionLabels[change.Action] + "定时：" + input.Title,
		Detail: detail, Kind: approval.Permission, Fields: fields,
		ConfirmLabel: actionLabels[change.Action] + "任务",
	})
	return answer.Allow, err
}

// routineFields are the card fields of routineFields in routines.ts.
func routineFields(item Routine) []approval.Field {
	when := routineTimeLabel(item.Schedule.Expression)
	if item.Schedule.Type == Once {
		when = item.Schedule.At
		if at, err := time.Parse(time.RFC3339, item.Schedule.At); err == nil {
			if location, err := time.LoadLocation(item.Schedule.TimeZone); err == nil {
				at = at.In(location)
			}
			when = at.Format("2006/1/2 15:04:05")
		}
	}
	delivery := "每次交付"
	if item.Delivery == Conditional {
		delivery = "满足条件才通知：" + item.Condition
	}
	minutes := strconv.FormatFloat(float64(cmp.Or(item.ExecutionMs, DefaultExecutionMs))/60_000, 'f', -1, 64)
	return []approval.Field{
		{Label: "执行时间", Value: when},
		{Label: "时区", Value: item.Schedule.TimeZone},
		{Label: "执行期限", Value: minutes + " 分钟（不含等待）"},
		{Label: "交付方式", Value: delivery},
	}
}

// routineTimeLabel is schedule-label.ts routineTimeLabel.
func routineTimeLabel(expression string) string {
	match := expressionPattern.FindStringSubmatch(expression)
	if match == nil {
		return expression
	}
	frequency := "每天"
	switch weekday := match[3]; weekday {
	case "*":
	case "1-5":
		frequency = "工作日"
	default:
		day, _ := strconv.Atoi(weekday)
		frequency = "每" + weekdays[day]
	}
	hour, _ := strconv.Atoi(match[2])
	minute, _ := strconv.Atoi(match[1])
	return fmt.Sprintf("%s %02d:%02d", frequency, hour, minute)
}

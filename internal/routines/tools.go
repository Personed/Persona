package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

// Register adds the routine_* product tools to a registry. routine_create and
// routine_manage change routines only after the shared approver accepts the call.
func Register(registry tools.Registry, service *Service) error {
	for _, tool := range []tools.Tool{
		&tool{definition: reportDefinition, run: service.runReport},
		&tool{definition: manageDefinition, run: service.runManage},
		&tool{definition: listDefinition, run: service.runList},
		&tool{definition: createDefinition, run: service.runCreate},
	} {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

type tool struct {
	definition tools.Definition
	run        func(ctx context.Context, call tools.Call) (any, error)
}

func (t *tool) Definition() tools.Definition { return t.definition }

func (t *tool) Run(ctx context.Context, call tools.Call) (tools.Result, error) {
	value, err := t.run(ctx, call)
	if routineError := (*Error)(nil); errors.As(err, &routineError) {
		return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: routineError.Message}}}, nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	text, err := json.Marshal(value)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: []tools.Content{{Type: "text", Text: string(text)}}}, nil
}

const scheduleSchema = `{"anyOf":[` +
	`{"type":"object","properties":{"type":{"const":"once"},"at":{"type":"string"},"timeZone":{"type":"string"}},"required":["type","at","timeZone"]},` +
	`{"type":"object","properties":{"type":{"const":"cron"},"expression":{"type":"string"},"timeZone":{"type":"string"}},"required":["type","expression","timeZone"]}]}`

// fieldsSchema holds the properties routine_create and routine_manage share.
var fieldsSchema = fmt.Sprintf(
	`"title":{"type":"string","minLength":1,"maxLength":%d},`+
		`"instructions":{"type":"string","minLength":1,"maxLength":%d},`+
		`"schedule":%s,`+
		`"skillIds":{"type":"array","items":{"type":"string"}},`+
		`"delivery":{"type":"string","enum":["always","conditional"]},`+
		`"condition":{"type":"string","minLength":1,"maxLength":%d},`+
		`"executionMs":{"type":"integer","minimum":%d,"maximum":%d}`,
	TitleLength, TextLength, scheduleSchema, TextLength, MinExecutionMs, MaxExecutionMs)

var (
	reportDefinition = tools.Definition{
		Name:        "routine_report",
		Description: "Submit this monitoring run's result. notify is true only when the agreed condition is met. Supply a stable source key when notifying. summary becomes the bounded next-run baseline after successful completion. Call exactly once after checking actual evidence.",
		InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"notify":{"type":"boolean"},"text":{"type":"string","maxLength":%d},"summary":{"type":"string","minLength":1,"maxLength":%d},"key":{"type":"string","minLength":1,"maxLength":%d}},"required":["notify","text","summary"]}`, TextLength, TextLength, KeyLength)),
	}
	manageDefinition = tools.Definition{
		Name:        "routine_manage",
		Description: "Propose a routine change for user confirmation. Read routine_list first. Supply its stable ID and current revision. Update only the fields that must change. Execution time excludes waiting for user input.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["update","pause","resume","run_now","delete"]},"id":{"type":"string"},"revision":{"type":"integer","minimum":0},` + fieldsSchema + `},"required":["action","id","revision"]}`),
		Mutates:     true,
	}
	listDefinition = tools.Definition{
		Name:        "routine_list",
		Description: "Read current host time, timezone, this Bot's routines and their run receipts.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: map[string]any{"readOnlyHint": true},
	}
	createDefinition = tools.Definition{
		Name:        "routine_create",
		Description: "Create a routine after user confirmation. Specify an explicit IANA timeZone. Once uses an ISO timestamp with offset. Cron uses five fields. Bind enabled skills by stable ID. An approval card confirms the schedule and instructions.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` + fieldsSchema + `},"required":["title","instructions","schedule"]}`),
		Mutates:     true,
	}
)

func source(call tools.Call) string { return fmt.Sprintf("bot:%d/tool:%s", call.BotID, call.CallID) }

func decode(input json.RawMessage, target any) error {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(input, target); err != nil {
		return &Error{BadRequest, "参数无效：" + err.Error()}
	}
	return nil
}

func (s *Service) runList(ctx context.Context, call tools.Call) (any, error) {
	return s.List(ctx, call.BotID)
}

func (s *Service) runCreate(ctx context.Context, call tools.Call) (any, error) {
	var change Change
	if err := decode(call.Input, &change); err != nil {
		return nil, err
	}
	change.Action, change.ID, change.Revision = "create", "", nil
	return s.Manage(ctx, call.BotID, change, call.CallID, source(call))
}

func (s *Service) runManage(ctx context.Context, call tools.Call) (any, error) {
	var change Change
	if err := decode(call.Input, &change); err != nil {
		return nil, err
	}
	if change.Action == "create" || change.ID == "" || change.Revision == nil {
		return nil, &Error{BadRequest, "参数无效：需要 action、id 和 revision。"}
	}
	return s.Manage(ctx, call.BotID, change, call.CallID, source(call))
}

func (s *Service) runReport(ctx context.Context, call tools.Call) (any, error) {
	var args struct {
		Notify  *bool   `json:"notify"`
		Text    *string `json:"text"`
		Summary string  `json:"summary"`
		Key     *string `json:"key"`
	}
	if err := decode(call.Input, &args); err != nil {
		return nil, err
	}
	if args.Notify == nil || args.Text == nil || textLength(*args.Text) > TextLength ||
		args.Summary == "" || textLength(args.Summary) > TextLength ||
		(args.Key != nil && (*args.Key == "" || textLength(*args.Key) > KeyLength)) {
		return nil, &Error{BadRequest, "参数无效：需要 notify、text 和 summary，且长度不得超限。"}
	}
	report := Report{Notify: *args.Notify, Text: *args.Text, Summary: args.Summary}
	if args.Key != nil {
		report.Key = *args.Key
	}
	if err := s.Report(ctx, call.BotID, store.TaskID(call.TaskID), report); err != nil {
		return nil, err
	}
	return map[string]bool{"accepted": true}, nil
}

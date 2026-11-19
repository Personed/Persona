package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jmespath/go-jmespath"

	"github.com/Personed/Persona/internal/tools"
)

const (
	// PreviewCharacters bounds each text block of a result sent to the model.
	PreviewCharacters = 8000
	// HeldResults is how many complete results stay queryable.
	HeldResults = 20
)

var queryDefinition = tools.Definition{
	Name:        "query_result",
	Description: "Query a held result with JMESPath over the complete value. Use the supplied shape and example. Filter nested arrays and select fields, e.g. apps[?contains(name, 'Music')].{name:name, path:launch_path}. Bound large answers with [0:20]. Read-only; never calls the original tool.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"result":{"type":"string"},"query":{"type":"string"}},"required":["result","query"],"additionalProperties":false}`),
	Annotations: map[string]any{"readOnlyHint": true, "idempotentHint": true},
}

// results holds the complete value of the gateway's latest results, so a bounded
// preview can be read further with query_result. It ports harness/tool-results.mjs.
type results struct {
	mu       sync.Mutex
	sequence int
	handles  []string
	held     map[string]any
}

// encode marshals a decoded JSON value without HTML escaping.
func encode(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

func parse(text string) (any, bool) {
	var value any
	return value, json.Unmarshal([]byte(text), &value) == nil
}

func keys(object map[string]any) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// fields names up to 12 keys of an object, in key order.
func fields(value any) []string {
	object, _ := value.(map[string]any)
	names := keys(object)
	return names[:min(len(names), 12)]
}

func first(items []any) any {
	for _, item := range items {
		if item != nil {
			return item
		}
	}
	return nil
}

// guide describes a held value's arrays and gives a query that runs against it.
func guide(value any) string {
	type list struct {
		key   string
		items []any
	}
	var lists []list
	switch typed := value.(type) {
	case []any:
		lists = []list{{"", typed}}
	case map[string]any:
		for _, key := range keys(typed) {
			if items, ok := typed[key].([]any); ok {
				lists = append(lists, list{key, items})
			}
		}
	}
	if len(lists) == 0 {
		kind := "object"
		switch value.(type) {
		case nil:
			kind = "null"
		case string:
			kind = "string"
		case float64:
			kind = "number"
		case bool:
			kind = "boolean"
		}
		return fmt.Sprintf("Shape: type: %s; keys: %s. Example: @", kind, strings.Join(fields(value), ", "))
	}
	shapes := make([]string, len(lists))
	for index, entry := range lists {
		name := entry.key
		if name == "" {
			name = "@"
		}
		shapes[index] = fmt.Sprintf("%s: %d items; fields: %s", name, len(entry.items), strings.Join(fields(first(entry.items)), ", "))
	}
	example := "[0:20]"
	if lists[0].key != "" {
		example = encode(lists[0].key) + example
	}
	if names := fields(first(lists[0].items)); len(names) > 0 {
		pairs := make([]string, min(len(names), 2))
		for index := range pairs {
			pairs[index] = encode(names[index]) + ":" + encode(names[index])
		}
		example += ".{" + strings.Join(pairs, ",") + "}"
	}
	return fmt.Sprintf("Shape: %s. Example: %s", strings.Join(shapes, "; "), example)
}

// bounded keeps whole array items and short fields while the character budget lasts,
// so the preview stays valid JSON.
func bounded(value any, remaining *int) any {
	switch typed := value.(type) {
	case []any:
		kept := []any{}
		for _, item := range typed {
			size := utf8.RuneCountInString(encode(item)) + 1
			if size > *remaining {
				break
			}
			*remaining -= size
			kept = append(kept, item)
		}
		return kept
	case map[string]any:
		object := make(map[string]any, len(typed))
		for _, key := range keys(typed) {
			object[key] = bounded(typed[key], remaining)
		}
		return object
	case string:
		if utf8.RuneCountInString(typed) > *remaining {
			return "[Read this field with query_result]"
		}
	}
	*remaining -= utf8.RuneCountInString(encode(value))
	return value
}

func preview(value any) string {
	remaining := PreviewCharacters
	return encode(bounded(value, &remaining))
}

// slim drops what the result text already carries from a structured value: long
// strings found in the text and the element tree of a desktop snapshot.
func slim(value any, texts []string) any {
	switch typed := value.(type) {
	case string:
		if utf8.RuneCountInString(typed) >= 256 && slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, typed) }) {
			return "[see result text]"
		}
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = slim(item, texts)
		}
		return items
	case map[string]any:
		object := make(map[string]any, len(typed))
		snapshot := truthy(typed["snapshot_id"])
		for key, item := range typed {
			if _, list := item.([]any); list && snapshot && key == "elements" {
				continue
			}
			object[key] = slim(item, texts)
		}
		return object
	}
	return value
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	}
	return true
}

// project holds a result's complete value and returns the result with each text block
// within the preview budget. A shortened result names its handle, shape and an example
// query. Structured content, images and resources pass through unchanged.
func (r *results) project(result tools.Result) tools.Result {
	var texts []string
	for _, part := range result.Content {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		}
	}
	var source, structured any
	if len(result.Structured) > 0 && json.Unmarshal(result.Structured, &structured) == nil {
		source = structured
	} else if value, ok := parse(strings.Join(texts, "\n")); ok {
		source = value
	} else {
		source = strings.Join(texts, "\n")
	}

	r.mu.Lock()
	r.sequence++
	handle := "r" + strconv.Itoa(r.sequence)
	if r.held == nil {
		r.held = map[string]any{}
	}
	r.held[handle] = source
	r.handles = append(r.handles, handle)
	if len(r.handles) > HeldResults {
		delete(r.held, r.handles[0])
		r.handles = r.handles[1:]
	}
	r.mu.Unlock()

	trimmed := false
	content := make([]tools.Content, 0, len(result.Content)+2)
	for _, part := range result.Content {
		if part.Type == "text" && utf8.RuneCountInString(part.Text) > PreviewCharacters {
			if value, ok := parse(part.Text); ok && value != nil {
				part.Text = preview(value)
			} else {
				part.Text = string([]rune(part.Text)[:PreviewCharacters])
			}
			trimmed = true
		}
		content = append(content, part)
	}
	if structured != nil {
		value := slim(structured, texts)
		repeated := slices.ContainsFunc(texts, func(text string) bool {
			parsed, ok := parse(text)
			return ok && reflect.DeepEqual(parsed, value)
		})
		if !repeated {
			text := encode(value)
			if utf8.RuneCountInString(text) > PreviewCharacters {
				text, trimmed = preview(value), true
			}
			content = append(content, tools.Content{Type: "text", Text: text})
		}
	}
	if trimmed {
		content = append(content, tools.Content{Type: "text", Text: fmt.Sprintf(`Partial result. Full records are held as %s. %s. Use query_result {"result":"%s","query":"<JMESPath>"}. Select only required records and fields. Do not search files or repeat an action to recover this result.`, handle, guide(source), handle)})
		if result.Handle == "" {
			result.Handle = handle
		}
	}
	result.Content = content
	return result
}

// query answers query_result from a held value. It never calls the original tool.
func (r *results) query(input json.RawMessage) tools.Result {
	failure := func(text string) tools.Result {
		return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: text}}}
	}
	var args struct {
		Result *string `json:"result"`
		Query  *string `json:"query"`
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil || args.Result == nil || args.Query == nil || strings.TrimSpace(*args.Query) == "" {
		return failure("参数无效：需要 result 和 query。")
	}
	r.mu.Lock()
	source, held := r.held[*args.Result]
	r.mu.Unlock()
	if !held {
		return failure("结果已失效。请读取原始工作记录。")
	}
	value, err := jmespath.Search(strings.TrimSpace(*args.Query), source)
	if err != nil {
		return failure(fmt.Sprintf("Invalid JMESPath: %s. %s", err, guide(source)))
	}
	return r.project(tools.Result{Content: []tools.Content{{Type: "text", Text: encode(value)}}})
}

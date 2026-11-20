package rpc

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Schema validates input as the TypeScript Zod schema does and reports Zod 4 issues
// with its English messages. It covers the JSON Schema 2020-12 subset that
// contracts/api/procedures.json uses, plus x-persona-trimmed and
// x-persona-url-protocols.
type Schema struct {
	typ          string
	properties   []string
	propSchemas  map[string]*Schema
	required     map[string]bool
	closed       bool
	additional   *Schema
	propertyName *Schema
	items        *Schema
	minItems     *json.Number
	maxItems     *json.Number
	minLength    *json.Number
	maxLength    *json.Number
	minimum      *json.Number
	maximum      *json.Number
	exclusiveMin *json.Number
	pattern      *regexp.Regexp
	patternText  string
	format       string
	enum         []any
	hasDefault   bool
	defaultValue any
	trimmed      *Schema
	protocols    []string
	allOf        []*Schema
	anyOf        []*Schema
	oneOf        []*Schema
	not          *Schema
}

// CompileSchema compiles a JSON Schema from procedures.json.
func CompileSchema(data json.RawMessage) (*Schema, error) {
	value, err := decode(data)
	if err != nil {
		return nil, err
	}
	return compile(value)
}

func compile(value any) (*Schema, error) {
	object, ok := value.(*Object)
	if !ok {
		return nil, fmt.Errorf("schema is %T, want object", value)
	}
	s := &Schema{}
	for _, key := range object.Keys {
		v := object.Values[key]
		var err error
		switch key {
		case "type":
			s.typ, ok = v.(string)
			if !ok {
				err = fmt.Errorf("type %v is not a string", v)
			}
		case "properties":
			props := v.(*Object)
			s.properties = props.Keys
			s.propSchemas = map[string]*Schema{}
			for _, name := range props.Keys {
				if s.propSchemas[name], err = compile(props.Values[name]); err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
			}
		case "required":
			s.required = map[string]bool{}
			for _, name := range v.([]any) {
				s.required[name.(string)] = true
			}
		case "additionalProperties":
			if b, isBool := v.(bool); isBool {
				s.closed = !b
			} else {
				s.additional, err = compile(v)
			}
		case "propertyNames":
			s.propertyName, err = compile(v)
		case "items":
			s.items, err = compile(v)
		case "minItems":
			s.minItems, err = number(v)
		case "maxItems":
			s.maxItems, err = number(v)
		case "minLength":
			s.minLength, err = number(v)
		case "maxLength":
			s.maxLength, err = number(v)
		case "minimum":
			s.minimum, err = number(v)
		case "maximum":
			s.maximum, err = number(v)
		case "exclusiveMinimum":
			s.exclusiveMin, err = number(v)
		case "pattern":
			s.patternText = v.(string)
			s.pattern, err = compilePattern(s.patternText)
		case "format":
			s.format = v.(string)
		case "enum":
			s.enum = v.([]any)
		case "const":
			s.enum = []any{v}
		case "default":
			s.hasDefault, s.defaultValue = true, v
		case "x-persona-trimmed":
			s.trimmed, err = compile(v)
		case "x-persona-url-protocols":
			for _, protocol := range v.([]any) {
				s.protocols = append(s.protocols, protocol.(string))
			}
		case "allOf", "anyOf", "oneOf":
			var list []*Schema
			for _, item := range v.([]any) {
				sub, subErr := compile(item)
				if subErr != nil {
					return nil, fmt.Errorf("%s: %w", key, subErr)
				}
				list = append(list, sub)
			}
			switch key {
			case "allOf":
				s.allOf = list
			case "anyOf":
				s.anyOf = list
			default:
				s.oneOf = list
			}
		case "not":
			s.not, err = compile(v)
		case "$schema", "contentEncoding", "description", "title":
		default:
			err = fmt.Errorf("unsupported keyword %q", key)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return s, nil
}

func number(v any) (*json.Number, error) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%v is not a number", v)
	}
	return &n, nil
}

// ecmaSpace is the ECMAScript WhiteSpace and LineTerminator set, which String.prototype.trim
// and the regular expression class \s use.
const ecmaSpace = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

const ecmaSpaceClass = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// compilePattern translates an ECMAScript pattern (no flags) to RE2: \uXXXX escapes and
// the Unicode \s and \S classes.
func compilePattern(source string) (*regexp.Regexp, error) {
	var out strings.Builder
	inClass := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch {
		case c == '\\' && i+1 < len(source):
			next := source[i+1]
			switch {
			case next == 'u' && i+5 < len(source):
				out.WriteString(`\x{` + source[i+2:i+6] + `}`)
				i += 5
			case next == 's' && inClass:
				out.WriteString(ecmaSpaceClass)
				i++
			case next == 's':
				out.WriteString("[" + ecmaSpaceClass + "]")
				i++
			case next == 'S' && !inClass:
				out.WriteString("[^" + ecmaSpaceClass + "]")
				i++
			default:
				out.WriteByte(c)
				out.WriteByte(next)
				i++
			}
		case c == '[' && !inClass:
			inClass = true
			out.WriteByte(c)
		case c == ']' && inClass:
			inClass = false
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return regexp.Compile(out.String())
}

// Issue is one Zod issue, finalized with path and message.
type Issue = *Object

func newIssue(path []any, fields ...any) Issue {
	issue := &Object{}
	for i := 0; i+1 < len(fields); i += 2 {
		issue.Set(fields[i].(string), fields[i+1])
	}
	if _, ok := issue.Get("path"); !ok {
		issue.Set("path", append([]any{}, path...))
	}
	issue.Set("message", issueMessage(issue))
	return issue
}

// issueMessage is the Zod 4 English locale.
func issueMessage(issue Issue) string {
	get := func(key string) any { v, _ := issue.Get(key); return v }
	text := func(key string) string { return fmt.Sprint(get(key)) }
	switch text("code") {
	case "invalid_type":
		return "Invalid input: expected " + text("expected") + ", received " + text("received")
	case "invalid_value":
		values := get("values").([]any)
		if len(values) == 1 {
			return "Invalid input: expected " + primitive(values[0])
		}
		parts := make([]string, len(values))
		for i, v := range values {
			parts[i] = primitive(v)
		}
		return "Invalid option: expected one of " + strings.Join(parts, "|")
	case "too_big", "too_small":
		big := text("code") == "too_big"
		adj := ">"
		bound := text("minimum")
		word := "small"
		if big {
			adj, bound, word = "<", text("maximum"), "big"
		}
		if get("inclusive") == true {
			adj += "="
		}
		origin := text("origin")
		units := map[string]string{"string": "characters", "file": "bytes", "array": "items"}
		if unit, ok := units[origin]; ok {
			return "Too " + word + ": expected " + origin + " to have " + adj + bound + " " + unit
		}
		return "Too " + word + ": expected " + origin + " to be " + adj + bound
	case "invalid_format":
		switch text("format") {
		case "regex":
			return "Invalid string: must match pattern " + text("pattern")
		case "uuid":
			return "Invalid UUID"
		case "url":
			return "Invalid URL"
		case "datetime":
			return "Invalid ISO datetime"
		}
		return "Invalid " + text("format")
	case "unrecognized_keys":
		keys := get("keys").([]any)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = primitive(k)
		}
		suffix := ""
		if len(keys) > 1 {
			suffix = "s"
		}
		return "Unrecognized key" + suffix + ": " + strings.Join(parts, ", ")
	case "invalid_key":
		return "Invalid key in " + text("origin")
	case "invalid_union":
		if options, ok := get("options").([]any); ok && len(options) > 0 {
			parts := make([]string, len(options))
			for i, o := range options {
				parts[i] = "'" + fmt.Sprint(o) + "'"
			}
			return "Invalid discriminator value. Expected " + strings.Join(parts, " | ")
		}
	}
	return "Invalid input"
}

func primitive(v any) string {
	if s, ok := v.(string); ok {
		data, _ := marshal(s)
		return string(data)
	}
	return fmt.Sprint(v)
}

func parsedType(v any) string {
	switch v.(type) {
	case undefinedValue:
		return "undefined"
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case *Blob:
		return "file"
	default:
		return "object"
	}
}

func invalidType(path []any, expected string, v any, extra ...any) Issue {
	fields := append([]any{"expected", expected}, extra...)
	fields = append(fields, "code", "invalid_type")
	issue := newIssue(path, fields...)
	// Zod derives "received" for the message only; the finalized issue omits it.
	issue.Values["message"] = "Invalid input: expected " + expected + ", received " + parsedType(v)
	return issue
}

// Validate checks input against s and returns the parsed value: defaults applied,
// trimmed strings trimmed and unknown keys of non-strict objects removed.
func (s *Schema) Validate(input any) (any, []Issue) {
	return s.validate(input, nil)
}

func (s *Schema) validate(v any, path []any) (any, []Issue) {
	if _, missing := v.(undefinedValue); missing && s.hasDefault {
		return s.defaultValue, nil
	}
	if len(s.anyOf) > 0 {
		return s.validateAnyOf(v, path)
	}
	if len(s.oneOf) > 0 {
		return s.validateOneOf(v, path)
	}
	var issues []Issue
	if s.enum != nil {
		for _, allowed := range s.enum {
			if sameJSON(allowed, v) {
				return v, nil
			}
		}
		return v, []Issue{newIssue(path, "code", "invalid_value", "values", s.enum)}
	}
	switch s.typ {
	case "string":
		if s.format == "binary" {
			return s.validateBlob(v, path)
		}
		str, ok := v.(string)
		if !ok {
			lengths := s
			if s.trimmed != nil {
				lengths = s.trimmed
			}
			return v, append([]Issue{invalidType(path, "string", v)}, lengths.lengthChecks(v, path)...)
		}
		if s.trimmed != nil {
			return s.trimmed.validate(strings.Trim(str, ecmaSpace), path)
		}
		v, issues = s.checkString(str, path)
	case "number", "integer":
		n, ok := v.(json.Number)
		if !ok {
			return v, []Issue{invalidType(path, "number", v)}
		}
		f, _ := n.Float64()
		if math.IsInf(f, 0) {
			received := "Infinity"
			if f < 0 {
				received = "-Infinity"
			}
			return v, []Issue{newIssue(path, "expected", "number", "code", "invalid_type", "received", received)}
		}
		if s.typ == "integer" && f != math.Trunc(f) {
			return v, []Issue{invalidType(path, "int", v, "format", "safeint")}
		}
		if s.minimum != nil && f < float(*s.minimum) {
			issues = append(issues, newIssue(path, "origin", "number", "code", "too_small", "minimum", *s.minimum, "inclusive", true))
		}
		if s.exclusiveMin != nil && f <= float(*s.exclusiveMin) {
			issues = append(issues, newIssue(path, "origin", "number", "code", "too_small", "minimum", *s.exclusiveMin, "inclusive", false))
		}
		if s.maximum != nil && f > float(*s.maximum) {
			issues = append(issues, newIssue(path, "origin", "number", "code", "too_big", "maximum", *s.maximum, "inclusive", true))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return v, []Issue{invalidType(path, "boolean", v)}
		}
	case "null":
		if v != nil {
			return v, []Issue{invalidType(path, "null", v)}
		}
	case "array":
		array, ok := v.([]any)
		if !ok {
			return v, append([]Issue{invalidType(path, "array", v)}, s.lengthChecks(v, path)...)
		}
		out := make([]any, len(array))
		for i, item := range array {
			out[i] = item
			if s.items != nil {
				var sub []Issue
				out[i], sub = s.items.validate(item, appendPath(path, i))
				issues = append(issues, sub...)
			}
		}
		issues = append(issues, s.lengthChecks(v, path)...)
		v = out
	case "object":
		object, ok := v.(*Object)
		if !ok {
			expected := "object"
			if s.additional != nil && s.properties == nil {
				expected = "record"
			}
			return v, []Issue{invalidType(path, expected, v)}
		}
		v, issues = s.validateObject(object, path)
	case "":
		if str, ok := v.(string); ok {
			v, issues = s.checkString(str, path)
		}
	default:
		return v, []Issue{invalidType(path, s.typ, v)}
	}
	// allOf holds the TypeScript refinements (z.custom and .refine), which report a
	// custom issue and are skipped once a type issue aborted the value.
	for _, sub := range s.allOf {
		if _, subIssues := sub.validate(v, path); len(subIssues) > 0 {
			issues = append(issues, newIssue(path, "code", "custom"))
		}
	}
	if s.not != nil {
		if _, sub := s.not.validate(v, path); len(sub) == 0 {
			issues = append(issues, newIssue(path, "code", "custom"))
		}
	}
	return v, issues
}

// lengthChecks runs the minimum and maximum length checks on any string or array.
// Zod runs them even after a type issue, because they apply to every value with a length.
func (s *Schema) lengthChecks(v any, path []any) []Issue {
	var length int
	var origin string
	switch v := v.(type) {
	case string:
		length, origin = utf16Length(v), "string"
	case []any:
		length, origin = len(v), "array"
	default:
		return nil
	}
	minimum, maximum := s.minLength, s.maxLength
	if s.typ == "array" {
		minimum, maximum = s.minItems, s.maxItems
	}
	var issues []Issue
	if minimum != nil && float64(length) < float(*minimum) {
		issues = append(issues, newIssue(path, "origin", origin, "code", "too_small", "minimum", *minimum, "inclusive", true))
	}
	if maximum != nil && float64(length) > float(*maximum) {
		issues = append(issues, newIssue(path, "origin", origin, "code", "too_big", "maximum", *maximum, "inclusive", true))
	}
	return issues
}

// checkString returns str as z.string() outputs it: z.url() trims it and
// removes tabs and newlines before the later checks see it.
func (s *Schema) checkString(str string, path []any) (string, []Issue) {
	var issues []Issue
	if s.format == "uri" {
		str = stripTabAndNewline(strings.Trim(str, ecmaSpace))
		if !canParseURL(str) {
			issues = append(issues, newIssue(path, "code", "invalid_format", "format", "url"))
		}
	}
	issues = append(issues, s.lengthChecks(str, path)...)
	switch s.format {
	case "uuid", "date-time":
		if s.pattern != nil && !s.pattern.MatchString(str) {
			format := map[string]string{"uuid": "uuid", "date-time": "datetime"}[s.format]
			issues = append(issues, newIssue(path, "origin", "string", "code", "invalid_format", "format", format, "pattern", "/"+s.patternText+"/"))
		}
	case "uri":
	default:
		if s.pattern != nil && !s.pattern.MatchString(str) {
			issues = append(issues, newIssue(path, "origin", "string", "code", "invalid_format", "format", "regex", "pattern", "/"+s.patternText+"/"))
		}
	}
	if len(s.protocols) > 0 {
		allowed := false
		for _, protocol := range s.protocols {
			allowed = allowed || (canParseURL(str) && urlScheme(str)+":" == protocol)
		}
		if !allowed {
			issues = append(issues, newIssue(path, "code", "custom"))
		}
	}
	return str, issues
}

func (s *Schema) validateBlob(v any, path []any) (any, []Issue) {
	blob, ok := v.(*Blob)
	if !ok {
		return v, []Issue{invalidType(path, "file", v)}
	}
	if s.maxLength != nil && float64(len(blob.Data)) > float(*s.maxLength) {
		return v, []Issue{newIssue(path, "origin", "file", "code", "too_big", "maximum", *s.maxLength, "inclusive", true)}
	}
	return v, nil
}

func (s *Schema) validateObject(object *Object, path []any) (any, []Issue) {
	var issues []Issue
	out := &Object{Values: map[string]any{}}
	for _, name := range s.properties {
		value, present := object.Get(name)
		if !present {
			value = undefined
		}
		sub := s.propSchemas[name]
		if !present && !s.required[name] && !sub.hasDefault {
			continue
		}
		parsed, subIssues := sub.validate(value, appendPath(path, name))
		issues = append(issues, subIssues...)
		if _, missing := parsed.(undefinedValue); !missing {
			out.Set(name, parsed)
		}
	}
	var unrecognized []any
	for _, key := range object.Keys {
		if _, known := s.propSchemas[key]; known {
			continue
		}
		switch {
		case s.closed:
			unrecognized = append(unrecognized, key)
		case s.additional != nil:
			if s.propertyName != nil {
				if _, keyIssues := s.propertyName.validate(key, nil); len(keyIssues) > 0 {
					issues = append(issues, newIssue(path, "code", "invalid_key", "origin", "record", "issues", keyIssues, "path", appendPath(path, key)))
					continue
				}
			}
			parsed, subIssues := s.additional.validate(object.Values[key], appendPath(path, key))
			issues = append(issues, subIssues...)
			out.Set(key, parsed)
		}
	}
	if len(unrecognized) > 0 {
		issues = append(issues, newIssue(path, "code", "unrecognized_keys", "keys", unrecognized))
	}
	return out, issues
}

func (s *Schema) validateAnyOf(v any, path []any) (any, []Issue) {
	// z.nullable() and z.optional() export as anyOf with a null branch; Zod reports the
	// inner schema's issues for them rather than a union issue.
	if len(s.anyOf) == 2 {
		for i, option := range s.anyOf {
			if option.typ == "null" && option.enum == nil {
				if v == nil {
					return nil, nil
				}
				return s.anyOf[1-i].validate(v, path)
			}
		}
	}
	errors := make([]any, 0, len(s.anyOf))
	for _, option := range s.anyOf {
		parsed, issues := option.validate(v, path)
		if len(issues) == 0 {
			return parsed, nil
		}
		errors = append(errors, relative(issues, len(path)))
	}
	return v, []Issue{newIssue(path, "code", "invalid_union", "errors", errors)}
}

func (s *Schema) validateOneOf(v any, path []any) (any, []Issue) {
	key, options := s.discriminator()
	object, ok := v.(*Object)
	if key == "" || !ok {
		if !ok {
			issue := newIssue(path, "code", "invalid_type", "expected", "object")
			issue.Values["message"] = "Invalid input: expected object, received " + parsedType(v)
			return v, []Issue{issue}
		}
		return s.validateAnyOf(v, path)
	}
	value, _ := object.Get(key)
	for i, option := range s.oneOf {
		for _, allowed := range option.propSchemas[key].enum {
			if sameJSON(allowed, value) {
				return s.oneOf[i].validate(v, path)
			}
		}
	}
	return v, []Issue{newIssue(path, "code", "invalid_union", "errors", []any{}, "note", "No matching discriminator",
		"discriminator", key, "options", options, "path", appendPath(path, key))}
}

// discriminator finds the property that has a const value in every oneOf branch.
func (s *Schema) discriminator() (string, []any) {
	first := s.oneOf[0]
	for _, key := range first.properties {
		var options []any
		for _, option := range s.oneOf {
			prop, ok := option.propSchemas[key]
			if !ok || len(prop.enum) == 0 {
				options = nil
				break
			}
			options = append(options, prop.enum...)
		}
		if options != nil {
			return key, options
		}
	}
	return "", nil
}

// relative strips the union's own path from nested issues, as Zod reports union
// member issues relative to the union.
func relative(issues []Issue, depth int) []Issue {
	out := make([]Issue, len(issues))
	for i, issue := range issues {
		copied := &Object{Keys: append([]string{}, issue.Keys...), Values: map[string]any{}}
		for k, v := range issue.Values {
			copied.Values[k] = v
		}
		if p, ok := copied.Values["path"].([]any); ok && len(p) >= depth {
			copied.Values["path"] = p[depth:]
		}
		out[i] = copied
	}
	return out
}

func appendPath(path []any, key any) []any {
	return append(append([]any{}, path...), key)
}

func float(n json.Number) float64 {
	f, _ := strconv.ParseFloat(string(n), 64)
	return f
}

func utf16Length(s string) int {
	length := 0
	for _, r := range s {
		length++
		if r > 0xffff {
			length++
		}
	}
	return length
}

func sameJSON(a, b any) bool {
	if an, ok := a.(json.Number); ok {
		bn, ok := b.(json.Number)
		return ok && float(an) == float(bn)
	}
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}

// Trim removes leading and trailing whitespace as String.prototype.trim does.
func Trim(s string) string { return strings.Trim(s, ecmaSpace) }

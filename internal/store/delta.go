package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"
)

func firstByte(value []byte) byte {
	if trimmed := bytes.TrimSpace(value); len(trimmed) > 0 {
		return trimmed[0]
	}
	return 0
}

func changeValue(current, base, ops json.RawMessage) (json.RawMessage, error) {
	switch {
	case len(base) != 0 && len(ops) == 0:
		return base, nil
	case len(base) == 0 && firstByte(ops) == '[':
		return applyDelta(current, ops)
	default:
		return nil, errors.New("document change requires exactly one base or delta")
	}
}

var errDeltaShape = errors.New("invalid Chord document operation")

// applyDelta consumes pi-durable's decoded Chord tuple ops. Materializing on
// write makes every historical read an indexed lookup, not an unbounded replay.
func applyDelta(base, delta json.RawMessage) (json.RawMessage, error) {
	var root any
	var operations [][]any
	for _, input := range []struct {
		raw  json.RawMessage
		dest any
	}{{base, &root}, {delta, &operations}} {
		d := json.NewDecoder(bytes.NewReader(input.raw))
		d.UseNumber()
		if !json.Valid(input.raw) {
			return nil, errDeltaShape
		}
		if err := d.Decode(input.dest); err != nil {
			return nil, err
		}
	}
	for i, op := range operations {
		if len(op) < 2 {
			return nil, errDeltaShape
		}
		verb, ok := op[0].(string)
		if !ok {
			return nil, errDeltaShape
		}
		if verb == "r" && len(op) == 2 {
			root = op[1]
			continue
		}
		path, ok := op[1].([]any)
		if !ok || (len(path) == 0 && verb != "p" && verb != "m") {
			return nil, errDeltaShape
		}
		var err error
		root, err = changePath(root, path, verb, op[2:])
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return json.Marshal(root)
}

func nonnegative(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	return int(n), err == nil && n >= 0 && n <= math.MaxInt
}

func changePath(node any, path []any, verb string, args []any) (any, error) {
	if len(path) == 0 {
		return changeNode(node, verb, args)
	}
	leaf := len(path) == 1
	switch parent := node.(type) {
	case map[string]any:
		key, ok := path[0].(string)
		if !ok {
			n, valid := nonnegative(path[0])
			if !valid {
				return nil, errDeltaShape
			}
			key = strconv.Itoa(n)
		}
		if key == "__proto__" || key == "constructor" || key == "prototype" {
			return nil, errDeltaShape
		}
		if leaf && verb == "d" && len(args) == 0 {
			delete(parent, key)
			return parent, nil
		}
		value, err := changePath(parent[key], path[1:], verb, args)
		if err != nil {
			return nil, err
		}
		parent[key] = value
		return parent, nil
	case []any:
		i, ok := nonnegative(path[0])
		if !ok || i > len(parent) {
			return nil, errDeltaShape
		}
		if i == len(parent) {
			if leaf && verb == "s" && len(args) == 1 {
				return append(parent, args[0]), nil
			}
			return nil, errDeltaShape
		}
		if leaf && verb == "d" && len(args) == 0 {
			return slices.Delete(parent, i, i+1), nil
		}
		value, err := changePath(parent[i], path[1:], verb, args)
		if err != nil {
			return nil, err
		}
		parent[i] = value
		return parent, nil
	default:
		return nil, errDeltaShape
	}
}

func changeNode(node any, verb string, args []any) (any, error) {
	switch {
	case verb == "s" && len(args) == 1:
		return args[0], nil
	case verb == "a" && len(args) == 1:
		text, ok := node.(string)
		suffix, valid := args[0].(string)
		if ok && valid {
			return text + suffix, nil
		}
	case verb == "t" && len(args) == 1:
		text, ok := node.(string)
		count, valid := nonnegative(args[0])
		if ok && valid {
			units := utf16.Encode([]rune(text))
			return string(utf16.Decode(units[min(count, len(units)):])), nil
		}
	case verb == "p" && len(args) == 3:
		array, ok := node.([]any)
		index, validIndex := nonnegative(args[0])
		remove, validRemove := nonnegative(args[1])
		items, validItems := args[2].([]any)
		if ok && validIndex && validRemove && validItems {
			index = min(index, len(array))
			return slices.Replace(array, index, index+min(remove, len(array)-index), items...), nil
		}
	case verb == "m" && len(args) == 1:
		array, ok := node.([]any)
		order, valid := args[0].([]any)
		if ok && valid && len(array) == len(order) {
			newOrder := make([]any, len(array))
			used := make([]bool, len(array))
			for i, position := range order {
				index, ok := nonnegative(position)
				if !ok || index >= len(array) || used[index] {
					return nil, errDeltaShape
				}
				newOrder[i], used[index] = array[index], true
			}
			return newOrder, nil
		}
	}
	return nil, errDeltaShape
}

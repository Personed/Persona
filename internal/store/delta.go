package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"
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
	if !utf8.Valid(base) || !utf8.Valid(delta) {
		return nil, errDeltaShape
	}
	root, err := decodeChord(base)
	if err != nil {
		return nil, err
	}
	var operations [][]json.RawMessage
	if err := json.Unmarshal(delta, &operations); err != nil {
		return nil, err
	}
	for i, op := range operations {
		if len(op) < 2 {
			return nil, errDeltaShape
		}
		var verb string
		if err := json.Unmarshal(op[0], &verb); err != nil {
			return nil, errDeltaShape
		}
		if verb == "r" {
			if len(op) != 2 {
				return nil, errDeltaShape
			}
			root, err = decodeChord(op[1])
		} else {
			var path []json.RawMessage
			if firstByte(op[1]) != '[' || json.Unmarshal(op[1], &path) != nil ||
				(len(path) == 0 && verb != "p" && verb != "m") {
				return nil, errDeltaShape
			}
			root, err = changePath(root, path, verb, op[2:])
		}
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", i, err)
		}
	}
	return appendChordJSON(nil, root), nil
}

func nonnegative(value json.RawMessage) (int, bool) {
	n, err := strconv.Atoi(string(value))
	return n, err == nil && n >= 0
}

func changePath(node any, path []json.RawMessage, verb string, args []json.RawMessage) (any, error) {
	if len(path) == 0 {
		return changeNode(node, verb, args)
	}
	leaf := len(path) == 1
	switch parent := node.(type) {
	case chordObject:
		var key string
		if firstByte(path[0]) == '"' {
			key = string(readChordString(path[0]).appendJSON(nil))
		} else {
			n, valid := nonnegative(path[0])
			if !valid {
				return nil, errDeltaShape
			}
			key = strconv.Quote(strconv.Itoa(n))
		}
		if key == `"__proto__"` || key == `"constructor"` || key == `"prototype"` {
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
				add, err := decodeChord(args[0])
				return append(parent, add), err
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

func changeNode(node any, verb string, args []json.RawMessage) (any, error) {
	switch {
	case verb == "s" && len(args) == 1:
		return decodeChord(args[0])
	case verb == "a" && len(args) == 1:
		text, ok := node.(chordString)
		if ok && firstByte(args[0]) == '"' {
			return append(text, readChordString(args[0])...), nil
		}
	case verb == "t" && len(args) == 1:
		text, ok := node.(chordString)
		count, valid := nonnegative(args[0])
		if ok && valid {
			return text[min(count, len(text)):], nil
		}
	case verb == "p" && len(args) == 3:
		array, ok := node.([]any)
		index, validIndex := nonnegative(args[0])
		remove, validRemove := nonnegative(args[1])
		if ok && validIndex && validRemove && firstByte(args[2]) == '[' {
			items, err := decodeChord(args[2])
			if err != nil {
				return nil, err
			}
			index = min(index, len(array))
			return slices.Replace(array, index, index+min(remove, len(array)-index), items.([]any)...), nil
		}
	case verb == "m" && len(args) == 1:
		array, ok := node.([]any)
		var order []json.RawMessage
		if ok && firstByte(args[0]) == '[' && json.Unmarshal(args[0], &order) == nil && len(array) == len(order) {
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

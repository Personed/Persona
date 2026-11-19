package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Object is a JSON object that keeps its key order, as JavaScript objects do. Zod
// reports issues and unrecognized keys in key order, so order is part of the wire.
type Object struct {
	Keys   []string
	Values map[string]any
}

// Set sets key, appending it when new.
func (o *Object) Set(key string, value any) {
	if o.Values == nil {
		o.Values = map[string]any{}
	}
	if _, ok := o.Values[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Values[key] = value
}

// Get returns the value at key.
func (o *Object) Get(key string) (any, bool) {
	value, ok := o.Values[key]
	return value, ok
}

// MarshalJSON writes the keys in order.
func (o *Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.Keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(name)
		buf.WriteByte(':')
		value, err := marshal(o.Values[key])
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// undefinedValue is JavaScript undefined: a missing property or an absent input.
type undefinedValue struct{}

var undefined any = undefinedValue{}

// decode parses JSON into *Object, []any, json.Number, string, bool and nil.
func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return value, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := &Object{Values: map[string]any{}}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				value, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				object.Set(key.(string), value)
			}
			_, err := dec.Token()
			return object, err
		case '[':
			array := []any{}
			for dec.More() {
				value, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			_, err := dec.Token()
			return array, err
		}
		return nil, fmt.Errorf("unexpected %v", token)
	default:
		return token, nil
	}
}

// marshal encodes like JSON.stringify: no HTML escaping of <, > and &.
func marshal(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

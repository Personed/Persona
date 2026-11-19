package store

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

type chordString []uint16

// readChordString accepts validated JSON. Chord works on JavaScript code units,
// so an unpaired surrogate must survive both a delta and a database round trip.
func readChordString(raw []byte) chordString {
	body := raw[1 : len(raw)-1]
	text := make(chordString, 0, len(body))
	for len(body) > 0 {
		if body[0] == '\\' {
			switch body[1] {
			case 'u':
				n, _ := strconv.ParseUint(string(body[2:6]), 16, 16)
				text, body = append(text, uint16(n)), body[6:]
				continue
			case 'b':
				text = append(text, '\b')
			case 'f':
				text = append(text, '\f')
			case 'n':
				text = append(text, '\n')
			case 'r':
				text = append(text, '\r')
			case 't':
				text = append(text, '\t')
			default:
				text = append(text, uint16(body[1]))
			}
			body = body[2:]
			continue
		}
		r, size := utf8.DecodeRune(body)
		if r <= 0xffff {
			text = append(text, uint16(r))
		} else {
			hi, lo := utf16.EncodeRune(r)
			text = append(text, uint16(hi), uint16(lo))
		}
		body = body[size:]
	}
	return text
}

func (s chordString) appendJSON(out []byte) []byte {
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		unit := rune(s[i])
		switch {
		case unit == '"' || unit == '\\':
			out = append(out, '\\', byte(unit))
		case i+1 < len(s) && unit >= 0xd800 && unit < 0xdc00 && s[i+1] >= 0xdc00 && s[i+1] <= 0xdfff:
			out = utf8.AppendRune(out, utf16.DecodeRune(unit, rune(s[i+1])))
			i++
		case unit < 0x20 || utf16.IsSurrogate(unit):
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', hex[unit>>12], hex[unit>>8&15], hex[unit>>4&15], hex[unit&15])
		default:
			out = utf8.AppendRune(out, unit)
		}
	}
	return append(out, '"')
}

// Object keys use canonical JSON string literals, preserving unpaired surrogates
// while making different spellings of the same Chord key compare equal.
type chordObject map[string]any

func decodeChord(raw json.RawMessage) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
	value, err := readChordValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadValue(); !errors.Is(err, io.EOF) {
		return nil, errDeltaShape
	}
	return value, nil
}

func readChordValue(dec *jsontext.Decoder) (any, error) {
	switch dec.PeekKind() {
	case '{':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		object := make(chordObject)
		for dec.PeekKind() == '"' {
			name, err := dec.ReadValue()
			if err != nil {
				return nil, err
			}
			key := string(readChordString(name).appendJSON(nil))
			value, err := readChordValue(dec)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		var array []any
		for dec.PeekKind() != ']' {
			value, err := readChordValue(dec)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		if raw.Kind() == '"' {
			return readChordString(raw), nil
		}
		return raw.Clone(), nil
	}
}

func appendChordJSON(out []byte, value any) []byte {
	switch v := value.(type) {
	case chordObject:
		out = append(out, '{')
		for i, key := range slices.Sorted(maps.Keys(v)) {
			if i != 0 {
				out = append(out, ',')
			}
			out = append(out, key...)
			out = appendChordJSON(append(out, ':'), v[key])
		}
		return append(out, '}')
	case []any:
		out = append(out, '[')
		for i, item := range v {
			if i != 0 {
				out = append(out, ',')
			}
			out = appendChordJSON(out, item)
		}
		return append(out, ']')
	case chordString:
		return v.appendJSON(out)
	case jsontext.Value:
		return append(out, v...)
	default:
		panic("invalid Chord JSON value")
	}
}

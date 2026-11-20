// Package worker runs on the Bot's work computer and serves commands, files and
// listings to the Persona service over stdin and stdout. It replaces the pi-env
// daemon and keeps its wire protocol (version 1) and file and command semantics.
package worker

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Protocol is the wire protocol version reported by hello.
const Protocol = 1

// MaxFrame bounds one frame, length field excluded.
const MaxFrame = 16 << 20

// Frame types.
const (
	frameRequest byte = 1
	frameResult  byte = 2
	frameError   byte = 3
	frameEvent   byte = 4
	frameCancel  byte = 5
	framePing    byte = 6
)

// syncPrefix starts the first line the worker writes. Shell startup files may print
// before it; readers discard everything up to the line.
const syncPrefix = "PERSONA-WORKER "

type frame struct {
	Type    byte
	ID      uint32
	JSON    json.RawMessage
	Payload []byte
}

// Error is a failed request. Code is a Node-style error code (ENOENT, EISDIR, ...)
// or an execution code (shell_unavailable, spawn_error, timeout, aborted, unknown).
// Only codes are part of the contract; messages are diagnostic.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Syscall string `json:"syscall,omitempty"`
	Path    string `json:"path,omitempty"`
	// Lost marks a request failed because the connection ended.
	Lost bool `json:"lost,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func readFrame(r *bufio.Reader) (frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length < 9 || length > MaxFrame {
		return frame{}, fmt.Errorf("worker: invalid frame length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return frame{}, err
	}
	jsonLength := binary.BigEndian.Uint32(body[5:9])
	if uint64(jsonLength) > uint64(length-9) {
		return frame{}, errors.New("worker: invalid JSON length")
	}
	f := frame{Type: body[0], ID: binary.BigEndian.Uint32(body[1:5])}
	f.JSON = body[9 : 9+jsonLength]
	f.Payload = body[9+jsonLength:]
	return f, nil
}

func encodeFrame(kind byte, id uint32, value any, payload []byte) ([]byte, error) {
	data := []byte("{}")
	if value != nil {
		var err error
		if data, err = json.Marshal(value); err != nil {
			return nil, err
		}
	}
	length := 9 + len(data) + len(payload)
	if length > MaxFrame {
		return nil, &Error{Code: "E2BIG", Message: "frame exceeds 16 MiB"}
	}
	buffer := make([]byte, 4+length)
	binary.BigEndian.PutUint32(buffer[0:4], uint32(length))
	buffer[4] = kind
	binary.BigEndian.PutUint32(buffer[5:9], id)
	binary.BigEndian.PutUint32(buffer[9:13], uint32(len(data)))
	copy(buffer[13:], data)
	copy(buffer[13+len(data):], payload)
	return buffer, nil
}

// Package rpc serves the oRPC wire recorded in contracts/api: POST /rpc/<path> with
// {"json": input} bodies, {"json": output} answers, oRPC error envelopes, and SSE
// event iterators for stream procedures.
package rpc

import "errors"

// Error is an oRPC error. It answers with Status and the body
// {"json":{"defined":false,"code","status","message","data"?}}.
type Error struct {
	Code    string
	Status  int
	Message string
	Data    any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// MarshalJSON writes the oRPC error JSON in the field order of ORPCError.toJSON.
func (e *Error) MarshalJSON() ([]byte, error) {
	type wire struct {
		Defined bool   `json:"defined"`
		Code    string `json:"code"`
		Status  int    `json:"status"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	}
	return marshal(wire{Code: e.Code, Status: e.Status, Message: e.Message, Data: e.Data})
}

type errorDef struct {
	status  int
	message string
}

// commonErrors mirrors COMMON_ORPC_ERROR_DEFS in @orpc/client.
var commonErrors = map[string]errorDef{
	"BAD_REQUEST":            {400, "Bad Request"},
	"UNAUTHORIZED":           {401, "Unauthorized"},
	"FORBIDDEN":              {403, "Forbidden"},
	"NOT_FOUND":              {404, "Not Found"},
	"METHOD_NOT_SUPPORTED":   {405, "Method Not Supported"},
	"NOT_ACCEPTABLE":         {406, "Not Acceptable"},
	"TIMEOUT":                {408, "Request Timeout"},
	"CONFLICT":               {409, "Conflict"},
	"PRECONDITION_FAILED":    {412, "Precondition Failed"},
	"PAYLOAD_TOO_LARGE":      {413, "Payload Too Large"},
	"UNSUPPORTED_MEDIA_TYPE": {415, "Unsupported Media Type"},
	"UNPROCESSABLE_CONTENT":  {422, "Unprocessable Content"},
	"TOO_MANY_REQUESTS":      {429, "Too Many Requests"},
	"CLIENT_CLOSED_REQUEST":  {499, "Client Closed Request"},
	"INTERNAL_SERVER_ERROR":  {500, "Internal Server Error"},
	"NOT_IMPLEMENTED":        {501, "Not Implemented"},
	"BAD_GATEWAY":            {502, "Bad Gateway"},
	"SERVICE_UNAVAILABLE":    {503, "Service Unavailable"},
	"GATEWAY_TIMEOUT":        {504, "Gateway Timeout"},
}

// NewError returns the oRPC error for code. An empty message uses the code's default
// message, as new ORPCError(code) does.
func NewError(code, message string) *Error {
	def, ok := commonErrors[code]
	status := 500
	if ok {
		status = def.status
	}
	if message == "" {
		message = def.message
		if message == "" {
			message = code
		}
	}
	return &Error{Code: code, Status: status, Message: message}
}

// WithData returns e with Data set.
func (e *Error) WithData(data any) *Error {
	e.Data = data
	return e
}

// toError converts err as toORPCError does: an *Error stays, anything else becomes
// INTERNAL_SERVER_ERROR without exposing its text.
func toError(err error) *Error {
	var rpcErr *Error
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return NewError("INTERNAL_SERVER_ERROR", "Internal server error")
}

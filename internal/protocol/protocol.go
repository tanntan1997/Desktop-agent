// Package protocol defines the JSON messages exchanged between the agent and
// the web server over the WebSocket tunnel. The same method names and error
// codes are used by the agent's local HTTP API (POST /api/rpc).
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Version is bumped on breaking changes to the message envelope.
const Version = 1

// Message types.
const (
	TypeHello    = "hello"    // agent -> server, first frame after connect
	TypeRequest  = "request"  // server -> agent
	TypeResponse = "response" // agent -> server, answers a request by ID
	TypeEvent    = "event"    // agent -> server, unsolicited notification
	TypeCancel   = "cancel"   // server -> agent, aborts an in-flight request by ID
)

// Message is the single envelope used for every frame on the tunnel.
type Message struct {
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Hello is the payload of the first frame the agent sends.
type Hello struct {
	Protocol int      `json:"protocol"`
	AgentID  string   `json:"agentId"`
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Hostname string   `json:"hostname"`
	Methods  []string `json:"methods"`
	Runners  []string `json:"runners"`
	Devices  any      `json:"devices"`
}

// Error codes.
const (
	CodeBadRequest   = "bad_request"
	CodeNotFound     = "not_found"
	CodeNotSupported = "not_supported"
	CodeUnavailable  = "unavailable"
	CodeBusy         = "busy"
	CodeForbidden    = "forbidden"
	CodeTimeout      = "timeout"
	CodeCanceled     = "canceled"
	CodeInternal     = "internal"
)

// Error is a typed RPC error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf builds an *Error with a formatted message.
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// AsError converts any error into an *Error. Wrapped *Error values keep their
// code but report the full wrapped message.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return &Error{Code: e.Code, Message: err.Error()}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Message: err.Error()}
	case errors.Is(err, context.Canceled):
		return &Error{Code: CodeCanceled, Message: err.Error()}
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// HTTPStatus maps an error code to the HTTP status used by the REST APIs.
func HTTPStatus(code string) int {
	switch code {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeForbidden:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeNotSupported:
		return http.StatusNotImplemented
	case CodeBusy:
		return http.StatusConflict
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

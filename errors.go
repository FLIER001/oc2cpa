package main

import (
	"fmt"
)

// ocError carries plugin-side error semantics across the RPC error envelope:
// Code is the client-visible error type label, HTTPStatus the status surfaced
// to the client (0 lets the host default to 500).
type ocError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *ocError) Error() string     { return e.Message }
func (e *ocError) ErrorCode() string { return e.Code }
func (e *ocError) StatusCode() int   { return e.HTTPStatus }

// zenStreamError marks plugin-side stream failures: no completion signal,
// upstream tool-call contract violation, malformed payloads. They surface as
// 502 upstream_stream_error, never 429.
type zenStreamError struct{ message string }

func (e *zenStreamError) Error() string     { return e.message }
func (e *zenStreamError) ErrorCode() string { return "upstream_stream_error" }
func (e *zenStreamError) StatusCode() int   { return 502 }

// mapUpstreamFailure mirrors upstreamFailure() in the original server: only a
// real upstream rate limit is 429; request/configuration problems surface as
// invalid_request_error so clients can tell "quota exhausted" from "wrong
// request" apart.
func mapUpstreamFailure(status int, message string) failure {
	text := message
	if text == "" {
		text = fmt.Sprintf("Upstream returned HTTP %d", status)
	}
	switch {
	case status == 429:
		return failure{
			Message: text + " (free model rate limit)",
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
			Status:  429,
		}
	case status == 401 || status == 403:
		return failure{Message: text, Type: "authentication_error", Code: "upstream_auth_error", Status: status}
	case status >= 400 && status < 500:
		// Upstream explicitly rejected the request (e.g. GLM rejects a name
		// field on role:"tool" messages): a client-side problem that must not
		// be mistaken for rate limiting and retried in a loop.
		return failure{Message: text, Type: "invalid_request_error", Code: "upstream_invalid_request", Status: status}
	default:
		return failure{Message: text, Type: "upstream_error", Code: "upstream_error", Status: status}
	}
}

// failure is the mapped oc2cap-style error block.
type failure struct {
	Message string
	Type    string
	Code    string
	Status  int
}

// asOCApiError converts a mapped failure into the RPC error carrier.
func (f failure) asError() *ocError {
	return &ocError{Code: f.Type, Message: f.Message, HTTPStatus: f.Status}
}

// streamFailureJSON is the SSE error-event payload shape of the original
// server, emitted inside the stream when the response headers are already
// committed (data: {"error":{...}} followed by stream close, without [DONE]).
func streamFailureJSON(message string) string {
	if message == "" {
		message = "Upstream stream error"
	}
	raw, _ := compactJSON(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    "upstream_stream_error",
		},
	})
	return raw
}

// upstreamErrorOf is the port of upstreamError(): extracts a readable message
// from the many shapes upstream error bodies take. Non-forced parsing only
// treats payloads carrying an "error" field or {"type":"error"} as failures so
// ordinary chat frames never match.
func upstreamErrorOf(parsed any, forced bool) (string, bool) {
	root, ok := parsed.(map[string]any)
	if !ok {
		return "", false
	}
	if !forced {
		_, hasError := root["error"]
		if !hasError {
			flag, isStr := root["type"].(string)
			if !isStr || flag != "error" {
				return "", false
			}
		}
	}
	message := ""
	switch errValue := root["error"].(type) {
	case string:
		message = errValue
	case map[string]any:
		if m, isStr := errValue["message"].(string); isStr {
			message = m
		}
	}
	if message == "" {
		if m, isStr := root["message"].(string); isStr {
			message = m
		}
	}
	if message == "" {
		message = "Upstream request failed"
	}
	return message, true
}

// compactJSON marshals without escaping issues for SSE frames.
func compactJSON(value any) (string, error) {
	raw, err := jsonMarshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

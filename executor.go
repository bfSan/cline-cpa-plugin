package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type upstreamStatusError struct {
	status  int
	message string
}

func (e *upstreamStatusError) Error() string { return e.message }

func clineUpstreamError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		if parsed.Error.Code != "" {
			message = parsed.Error.Code + ": " + parsed.Error.Message
		} else {
			message = parsed.Error.Message
		}
	}
	if status == http.StatusForbidden && strings.Contains(message, "ENTITLEMENT_ERROR") {
		return &upstreamStatusError{
			status:  status,
			message: "ClinePass subscription is not active for this account; subscribe or use a free model",
		}
	}
	// Cline answers 500 `empty response content` when the request itself leaves
	// no room for a reply — observed with reasoning models where max_tokens is
	// consumed by the reasoning phase, so the visible content comes back empty.
	// That is a client-side parameter problem, not an upstream outage, and CPA
	// cools credentials down on 5xx. Report 400 so one bad request cannot take
	// the whole account out of rotation.
	if isEmptyContentError(message) {
		return &upstreamStatusError{
			status:  http.StatusBadRequest,
			message: "upstream produced no content: " + truncate(message, 200) + " (try a larger max_tokens)",
		}
	}
	return &upstreamStatusError{
		status:  status,
		message: fmt.Sprintf("upstream %d: %s", status, truncate(message, 240)),
	}
}

// isEmptyContentError matches Cline's "the request produced nothing" body.
func isEmptyContentError(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "empty response content")
}

func resolveUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	return model
}

// normalizeUpstreamResponse flattens Cline's non-streaming envelope. The
// chat/completions endpoint returns {"data":{"choices":...}} while OpenAI
// compatible CPA clients expect choices at the top level. Streaming responses
// are already emitted as standard chat.completion.chunk frames.
func normalizeUpstreamResponse(body []byte) []byte {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Data) == 0 {
		return body
	}
	var direct struct {
		Choices json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &direct); err == nil && len(direct.Choices) > 0 {
		return body
	}
	var nested struct {
		Choices json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(envelope.Data, &nested); err != nil || len(nested.Choices) == 0 {
		return body
	}
	return envelope.Data
}

func newUpstreamChatRequest(payload []byte, sa *storedAuth, model string) (*http.Request, error) {
	if sa == nil {
		return nil, fmt.Errorf("stored auth is nil")
	}
	body := payload
	var parsed map[string]any
	if json.Unmarshal(payload, &parsed) == nil {
		parsed["model"] = resolveUpstreamModel(model)
		if _, ok := parsed["stream"]; !ok {
			parsed["stream"] = true
		}
		if rewritten, err := json.Marshal(parsed); err == nil {
			body = rewritten
		}
	}
	req, err := http.NewRequest(http.MethodPost, clineAPIBase+"/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = clineHeaders(sa.Auth.AccessToken)
	return req, nil
}

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	var parsed map[string]any
	if json.Unmarshal(payload, &parsed) == nil {
		parsed["stream"] = false
		if rewritten, err := json.Marshal(parsed); err == nil {
			payload = rewritten
		}
	}
	cred := newCredential(sa, req.AuthAttributes)
	resp, err := cred.do(func(current *storedAuth) (*http.Request, error) {
		httpReq, errReq := newUpstreamChatRequest(payload, current, req.Model)
		if errReq != nil {
			return nil, errReq
		}
		httpReq.Header.Set("Accept", "application/json")
		return httpReq, nil
	})
	if err != nil {
		if statusError, ok := asUpstreamStatusError(err); ok {
			return errorEnvelopeWithStatus("http_error", statusError.message, statusError.status), nil
		}
		return nil, err
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		upstreamErr := clineUpstreamError(resp.StatusCode, body)
		status := resp.StatusCode
		if statusError, ok := upstreamErr.(*upstreamStatusError); ok {
			status = statusError.status
		}
		return errorEnvelopeWithStatus("http_error", upstreamErr.Error(), status), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: normalizeUpstreamResponse(body)})
}

type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	var parsed map[string]any
	if json.Unmarshal(payload, &parsed) == nil {
		parsed["stream"] = true
		if rewritten, err := json.Marshal(parsed); err == nil {
			payload = rewritten
		}
	}
	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)
	cred := newCredential(sa, req.AuthAttributes)
	if req.StreamID == "" {
		chunks, status, err := collectUpstreamStream(payload, cred, req.Model, sseFramed)
		if err != nil {
			if statusError, ok := err.(*upstreamStatusError); ok {
				return errorEnvelopeWithStatus("http_error", statusError.message, statusError.status), nil
			}
			return errorEnvelopeWithStatus("http_error", err.Error(), status), nil
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}
	go pumpUpstreamStream(payload, cred, req.StreamID, sseFramed, req.Model, time.Now())
	return okEnvelope(streamResponse{Headers: headers})
}

// asUpstreamStatusError unwraps the typed upstream errors the executor raises so
// the host sees the right status instead of a generic failure.
func asUpstreamStatusError(err error) (*upstreamStatusError, bool) {
	var statusError *upstreamStatusError
	if errors.As(err, &statusError) {
		return statusError, true
	}
	return nil, false
}

func validatePayloadJSON(payload []byte) error {
	if len(payload) == 0 || !json.Valid(payload) {
		return fmt.Errorf("invalid JSON payload")
	}
	return nil
}

var _ = io.Discard

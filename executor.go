package main

import (
	"bytes"
	"encoding/json"
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
	return &upstreamStatusError{
		status:  status,
		message: fmt.Sprintf("upstream %d: %s", status, truncate(message, 240)),
	}
}

func resolveUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	return model
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
	httpReq, err := newUpstreamChatRequest(payload, sa, req.Model)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
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
	return okEnvelope(pluginapi.ExecutorResponse{Payload: body})
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
	if req.StreamID == "" {
		chunks, status, err := collectUpstreamStream(payload, sa, req.Model, sseFramed)
		if err != nil {
			if statusError, ok := err.(*upstreamStatusError); ok {
				return errorEnvelopeWithStatus("http_error", statusError.message, statusError.status), nil
			}
			return errorEnvelopeWithStatus("http_error", err.Error(), status), nil
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}
	httpReq, err := newUpstreamChatRequest(payload, sa, req.Model)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	go pumpUpstreamStream(httpReq, nil, req.StreamID, sseFramed, req.Model, time.Now())
	return okEnvelope(streamResponse{Headers: headers})
}

func validatePayloadJSON(payload []byte) error {
	if len(payload) == 0 || !json.Valid(payload) {
		return fmt.Errorf("invalid JSON payload")
	}
	return nil
}

var _ = io.Discard

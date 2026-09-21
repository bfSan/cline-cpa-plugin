package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func emptyStreamError() error {
	return fmt.Errorf("empty upstream stream (unexpected EOF)")
}

func upstreamReadError(err error) error {
	return fmt.Errorf("upstream stream read error (unexpected EOF): %w", err)
}

func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamErrorFrame(streamID, message string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"stream_id": streamID,
		"error":     message,
	})
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	body, err := streamErrorFrame(streamID, truncate(message, 600))
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostStreamEmit, body)
}

var streamCloseOnce sync.Map

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	actual, _ := streamCloseOnce.LoadOrStore(streamID, &sync.Once{})
	actual.(*sync.Once).Do(func() {
		body, _ := json.Marshal(map[string]string{"stream_id": streamID})
		_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
	})
}

func streamHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("X-Accel-Buffering", "no")
	return headers
}

func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

func pumpUpstreamStream(req *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel string, started time.Time) {
	defer streamClose(streamID)
	if cancel != nil {
		defer cancel()
	}
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		streamEmitError(streamID, clineUpstreamError(resp.StatusCode, body).Error())
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	emitted := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		if !json.Valid([]byte(data)) {
			continue
		}
		payload := data
		if sseFramed {
			payload = "data: " + payload + "\n\n"
		}
		if err := streamEmit(streamID, []byte(payload)); err != nil {
			return
		}
		emitted = true
	}
	if err := scanner.Err(); err != nil {
		streamEmitError(streamID, upstreamReadError(err).Error())
		return
	}
	if !emitted {
		streamEmitError(streamID, emptyStreamError().Error())
	}
	_ = requestedModel
	_ = started
}

func collectUpstreamStream(body []byte, sa *storedAuth, model string, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, int, error) {
	httpReq, err := newUpstreamChatRequest(body, sa, model)
	if err != nil {
		return nil, 0, err
	}
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, clineUpstreamError(resp.StatusCode, errBody)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	emitted := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		if !json.Valid([]byte(data)) {
			continue
		}
		payload := data
		if sseFramed {
			payload = "data: " + payload + "\n\n"
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
		emitted = true
	}
	if err := scanner.Err(); err != nil {
		return chunks, resp.StatusCode, upstreamReadError(err)
	}
	if !emitted {
		return chunks, resp.StatusCode, emptyStreamError()
	}
	return chunks, resp.StatusCode, nil
}

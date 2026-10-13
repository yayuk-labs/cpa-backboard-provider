package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	t "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"net/http"
	"strings"
	"time"
)

func (a *App) executeStream(req request) (any, error) {
	if req.StreamID == "" {
		return nil, fail(400, "CPA stream_id is required")
	}
	c, err := a.prepare(req, true)
	if err != nil {
		return nil, err
	}
	payload, err := a.httpRequest(c.req, "POST", c.path, c.body)
	if err != nil {
		a.remove(c.conversation)
		return nil, err
	}
	var upstream struct {
		Status int    `json:"status_code"`
		ID     string `json:"stream_id"`
	}
	if err = a.host("host.http.do_stream", payload, &upstream); err != nil {
		a.remove(c.conversation)
		return nil, err
	}
	if upstream.Status < 200 || upstream.Status >= 300 || upstream.ID == "" {
		if upstream.ID != "" {
			a.host("host.http.stream_close", map[string]string{"stream_id": upstream.ID}, nil)
		}
		a.remove(c.conversation)
		return nil, upstreamError(upstream.Status)
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.host("host.http.stream_close", map[string]string{"stream_id": upstream.ID}, nil)
		a.remove(c.conversation)
		return nil, fail(503, "Backboard plugin is shutting down")
	}
	a.streams[upstream.ID] = req.StreamID
	a.workers.Add(1)
	a.mu.Unlock()
	go a.pump(c, upstream.ID)
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}}, nil
}

func (a *App) pump(c *completion, upstreamID string) {
	defer a.workers.Done()
	var streamErr error
	success := false
	defer func() {
		a.host("host.http.stream_close", map[string]string{"stream_id": upstreamID}, nil)
		if !success {
			a.remove(c.conversation)
		}
		message := ""
		if streamErr != nil {
			message = "Backboard stream failed"
			if rpcErr, ok := streamErr.(*RPCError); ok {
				message = rpcErr.Message
			}
		}
		a.host("host.stream.close", map[string]string{"stream_id": c.req.StreamID, "error": message}, nil)
		a.mu.Lock()
		delete(a.streams, upstreamID)
		a.mu.Unlock()
	}()
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	var state any
	send := func(raw []byte) error {
		frames := [][]byte{raw}
		if c.format != t.FormatOpenAI {
			frames = t.TranslateStream(context.Background(), t.FormatOpenAI, c.format, c.model, c.req.OriginalRequest, c.req.Payload, append([]byte("data: "), raw...), &state)
		}
		for _, frame := range frames {
			if len(frame) > 0 {
				if err := a.host("host.stream.emit", map[string]any{"stream_id": c.req.StreamID, "payload": frame}, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
	emit := func(delta map[string]any, finish any, usage any) error {
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": c.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			chunk["usage"] = usage
		}
		raw, _ := json.Marshal(chunk)
		return send(raw)
	}
	if streamErr = emit(map[string]any{"role": "assistant", "content": ""}, nil, nil); streamErr != nil {
		return
	}
	var content, reasoning strings.Builder
	process := func(data []byte, eventType string) (bool, error) {
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return false, nil
		}
		var event struct {
			result
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false, fail(502, "Invalid Backboard SSE event")
		}
		if event.Type == "" {
			event.Type = eventType
		}
		switch event.Type {
		case "content_streaming":
			content.WriteString(event.Content)
			return false, emit(map[string]any{"content": event.Content}, nil, nil)
		case "reasoning_streaming":
			reasoning.WriteString(event.Content)
			return false, emit(map[string]any{"reasoning_content": event.Content}, nil, nil)
		case "error", "run_failed":
			return false, fail(502, "Backboard run failed")
		case "tool_submit_required", "run_ended":
			event.Content = content.String()
			event.Reasoning = reasoning.String()
			finish := "stop"
			delta := map[string]any{}
			if event.Type == "tool_submit_required" {
				event.Status = "REQUIRES_ACTION"
				finish = "tool_calls"
				calls := make([]map[string]any, 0, len(event.Calls))
				for index, call := range event.Calls {
					calls = append(calls, map[string]any{"index": index, "id": call.ID, "type": "function", "function": call.Function})
				}
				delta["tool_calls"] = calls
			} else if event.Status == "" {
				event.Status = "COMPLETED"
			}
			event.result = event.result.normalized()
			if err := validateResult(event.result); err != nil {
				return false, err
			}
			if err := emit(delta, finish, event.usage()); err != nil {
				return false, err
			}
			// CPA adds OpenAI SSE framing and [DONE]; other formats use SDK framing.
			if c.format != t.FormatOpenAI {
				if err := send([]byte("[DONE]")); err != nil {
					return false, err
				}
			}
			a.remember(c, event.result)
			success = true
			return true, nil
		}
		return false, nil
	}
	var buffer, data []byte
	eventType := ""
	line := func(raw []byte) (bool, error) {
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		if len(raw) == 0 {
			done, err := process(bytes.TrimSuffix(data, []byte("\n")), eventType)
			data = nil
			eventType = ""
			return done, err
		}
		if bytes.HasPrefix(raw, []byte("data:")) {
			part := bytes.TrimPrefix(raw[5:], []byte(" "))
			data = append(data, part...)
			data = append(data, '\n')
		}
		if bytes.HasPrefix(raw, []byte("event:")) {
			eventType = strings.TrimSpace(string(raw[6:]))
		}
		return false, nil
	}
	for {
		var chunk struct {
			Payload []byte `json:"payload"`
			Done    bool   `json:"done"`
			Error   string `json:"error"`
		}
		if streamErr = a.host("host.http.stream_read", map[string]string{"stream_id": upstreamID}, &chunk); streamErr != nil {
			return
		}
		buffer = append(buffer, chunk.Payload...)
		for {
			index := bytes.IndexByte(buffer, '\n')
			if index < 0 {
				break
			}
			done, err := line(buffer[:index])
			buffer = buffer[index+1:]
			if err != nil {
				streamErr = err
				return
			}
			if done {
				return
			}
			if len(data) > 16*1024*1024 {
				streamErr = fail(502, "Backboard SSE event exceeds size limit")
				return
			}
		}
		if len(buffer) > 16*1024*1024 || content.Len()+reasoning.Len() > 16*1024*1024 {
			streamErr = fail(502, "Backboard stream exceeds size limit")
			return
		}
		if chunk.Error != "" {
			streamErr = fail(502, "Backboard upstream stream failed")
			return
		}
		if chunk.Done {
			if len(buffer) > 0 {
				if done, err := line(buffer); err != nil {
					streamErr = err
					return
				} else if done {
					return
				}
			}
			if done, err := process(bytes.TrimSuffix(data, []byte("\n")), eventType); err != nil {
				streamErr = err
				return
			} else if done {
				return
			}
			streamErr = fail(502, "Backboard stream ended before a terminal event")
			return
		}
	}
}

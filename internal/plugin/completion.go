package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	t "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

const threadTTL = 30 * time.Minute
const maxThreads = 256

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type conversation struct {
	req                               request
	assistantID, threadID, historyKey string
	updated                           time.Time
	busy                              bool
	pending                           []toolCall
}
type completion struct {
	req                    request
	model, provider, scope string
	messages               []message
	system                 string
	tools                  []json.RawMessage
	conversation           *conversation
	body                   map[string]any
	path                   string
	format                 t.Format
}

func hash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value, nil
	}
	var blocks []struct {
		Type string
		Text string
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", fail(400, "Message content must be text")
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fail(400, "Backboard plugin supports text content only")
		}
		text.WriteString(block.Text)
	}
	return text.String(), nil
}

func (a *App) prepare(req request, stream bool) (*completion, error) {
	var payload struct {
		Model    string
		Messages []struct {
			Role       string
			Content    json.RawMessage
			ToolCalls  []toolCall `json:"tool_calls"`
			ToolCallID string     `json:"tool_call_id"`
		}
		Tools      []json.RawMessage
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(req.Payload, &payload) != nil || len(payload.Messages) == 0 {
		return nil, fail(400, "Completion requires a nonempty messages array")
	}
	if req.SourceFormat != "" && req.SourceFormat != "openai" && req.SourceFormat != "chat-completions" {
		return nil, fail(400, "CPA must translate input to chat-completions")
	}
	model := req.Model
	if model == "" {
		model = payload.Model
	}
	provider, name, ok := strings.Cut(strings.TrimPrefix(model, "backboard/"), "/")
	if !ok || provider == "" || name == "" {
		return nil, fail(400, "Use a provider-qualified model ID from /v1/models")
	}
	c := &completion{req: req, model: model, provider: provider, tools: payload.Tools, path: "/threads/messages"}
	c.format = t.FromString(req.Format)
	if c.format == "" || c.format == "chat-completions" {
		c.format = t.FormatOpenAI
	}
	if c.format != t.FormatOpenAI && c.format != t.FormatClaude && c.format != t.FormatOpenAIResponse {
		return nil, fail(400, "Unsupported CPA output format")
	}
	for _, m := range payload.Messages {
		text, err := contentText(m.Content)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "system", "developer":
			if c.system != "" {
				c.system += "\n\n"
			}
			c.system += text
		case "user", "assistant", "tool":
			c.messages = append(c.messages, message{m.Role, text, m.ToolCalls, m.ToolCallID})
		default:
			return nil, fail(400, "Unsupported message role")
		}
	}
	if len(c.messages) == 0 {
		return nil, fail(400, "At least one conversation message is required")
	}
	if last := c.messages[len(c.messages)-1]; last.Role != "user" && last.Role != "tool" {
		return nil, fail(400, "The final message must be user or tool")
	}
	if len(payload.ToolChoice) > 0 && string(payload.ToolChoice) != "null" {
		var choice string
		if json.Unmarshal(payload.ToolChoice, &choice) != nil || (choice != "auto" && choice != "none") {
			return nil, fail(400, "Backboard supports tool_choice auto or none only")
		}
		if choice == "none" {
			c.tools = nil
		}
	}
	for _, raw := range c.tools {
		var tool struct {
			Type     string
			Function struct{ Name string }
		}
		if json.Unmarshal(raw, &tool) != nil || tool.Type != "function" || tool.Function.Name == "" {
			return nil, fail(400, "Tools must use OpenAI function definitions")
		}
	}
	// Normalize absent assistant content and function types for client round trips.
	for i := range c.messages {
		for j := range c.messages[i].ToolCalls {
			tc := &c.messages[i].ToolCalls[j]
			if tc.Type == "" {
				tc.Type = "function"
			}
			var args any
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
				return nil, fail(400, "Tool arguments must be valid JSON")
			}
			raw, _ := json.Marshal(args)
			tc.Function.Arguments = string(raw)
		}
	}
	metadata := req.AuthMetadata
	if len(metadata) == 0 {
		metadata = req.Metadata
	}
	key, err := apiKey(req.StorageJSON, metadata)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	base := a.baseURL
	a.mu.Unlock()
	req.baseURL = base
	c.req = req
	c.scope = hash([]any{base, key, provider, name, c.system, c.tools})
	lastUser := len(c.messages) - 1
	for lastUser >= 0 && c.messages[lastUser].Role == "tool" {
		lastUser--
	}
	prefix := c.messages[:lastUser+1]
	if lastUser == len(c.messages)-1 {
		prefix = c.messages[:lastUser]
	}
	historyKey := hash([]any{c.scope, prefix})
	a.mu.Lock()
	var expired []*conversation
	for entry := range a.conversations {
		if !entry.busy && time.Since(entry.updated) > threadTTL {
			entry.busy = true
			delete(a.history, entry.historyKey)
			expired = append(expired, entry)
		}
	}
	conv := a.history[historyKey]
	if conv != nil && !conv.busy {
		conv.busy = true
		delete(a.history, historyKey)
	} else {
		conv = nil
	}
	a.mu.Unlock()
	for _, entry := range expired {
		a.remove(entry)
	}
	if conv == nil {
		if c.messages[len(c.messages)-1].Role == "tool" {
			return nil, fail(409, "Tool continuation is unavailable: retain full history and the same credential, model, system, and tools")
		}
		a.mu.Lock()
		if len(a.conversations) >= maxThreads {
			a.mu.Unlock()
			return nil, fail(503, "Backboard thread cache is full; retry after idle threads expire")
		}
		stored, _ := json.Marshal(map[string]string{"api_key": key})
		conv = &conversation{req: request{StorageJSON: stored, baseURL: base}, updated: time.Now(), busy: true}
		a.conversations[conv] = true
		a.mu.Unlock()
		var assistant struct {
			ID string `json:"assistant_id"`
		}
		if err = a.http(req, "POST", "/assistants", map[string]any{"name": "cpa-backboard-provider", "system_prompt": c.system, "tools": c.tools}, &assistant); err != nil {
			a.remove(conv)
			return nil, err
		}
		if assistant.ID == "" {
			a.remove(conv)
			return nil, fail(502, "Backboard omitted assistant_id")
		}
		conv.assistantID = assistant.ID
		var thread struct {
			ID string `json:"thread_id"`
		}
		if err = a.http(req, "POST", "/assistants/"+url.PathEscape(assistant.ID)+"/threads", nil, &thread); err != nil {
			a.remove(conv)
			return nil, err
		}
		if thread.ID == "" {
			a.remove(conv)
			return nil, fail(502, "Backboard omitted thread_id")
		}
		conv.threadID = thread.ID
		lastUser = -1
	}
	c.conversation = conv
	if c.messages[len(c.messages)-1].Role == "tool" {
		outputs := make([]map[string]string, 0, len(conv.pending))
		pending := make(map[string]bool, len(conv.pending))
		for _, call := range conv.pending {
			pending[call.ID] = true
		}
		for _, m := range c.messages[lastUser+1:] {
			if !pending[m.ToolCallID] {
				a.release(conv)
				return nil, fail(400, "Tool result ID is unknown or duplicated")
			}
			delete(pending, m.ToolCallID)
			outputs = append(outputs, map[string]string{"tool_call_id": m.ToolCallID, "output": m.Content})
		}
		if len(pending) > 0 || len(outputs) == 0 {
			a.release(conv)
			return nil, fail(400, "Submit results for every pending tool call together")
		}
		c.path = "/threads/tool-outputs"
		c.body = map[string]any{"thread_id": conv.threadID, "tool_outputs": outputs, "stream": stream}
		return c, nil
	}
	if len(conv.pending) > 0 {
		a.release(conv)
		return nil, fail(400, "Submit pending tool results before another user message")
	}
	content := c.messages[len(c.messages)-1].Content
	if lastUser == -1 {
		// shortcut: cold history becomes a labeled transcript; use a role-aware API if Backboard adds one.
		if len(c.messages) > 1 {
			raw, _ := json.Marshal(c.messages)
			content = "Conversation history (JSON, chronological roles):\n" + string(raw) + "\nRespond to the final user message."
		}
	}
	c.body = map[string]any{"thread_id": conv.threadID, "content": content, "system_prompt": c.system, "tools": c.tools, "model_name": name, "llm_provider": provider, "memory": "off", "stream": stream}
	return c, nil
}

type result struct {
	Status     string     `json:"status"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning"`
	Calls      []toolCall `json:"tool_calls"`
	Input      int64      `json:"input_tokens"`
	Cached     int64      `json:"cached_input_tokens"`
	CacheWrite int64      `json:"cache_write_input_tokens"`
	Output     int64      `json:"output_tokens"`
}

func (r result) normalized() result {
	r.Status = strings.ToUpper(r.Status)
	// Completed tool-output responses can retain historical calls.
	if r.Status == "COMPLETED" {
		r.Calls = nil
	}
	return r
}

func (r result) usage() map[string]any {
	prompt := r.Input + r.Cached + r.CacheWrite
	return map[string]any{"prompt_tokens": prompt, "completion_tokens": r.Output, "total_tokens": prompt + r.Output, "prompt_tokens_details": map[string]int64{"cached_tokens": r.Cached, "cache_write_tokens": r.CacheWrite}}
}
func (r result) message() message {
	return message{Role: "assistant", Content: r.Content, ToolCalls: r.Calls}
}
func (a *App) execute(req request) (any, error) {
	c, err := a.prepare(req, false)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			a.remove(c.conversation)
		}
	}()
	var response result
	if err = a.http(c.req, "POST", c.path, c.body, &response); err != nil {
		return nil, err
	}
	response = response.normalized()
	if err = validateResult(response); err != nil {
		return nil, err
	}
	a.remember(c, response)
	finish := "stop"
	if len(response.Calls) > 0 {
		finish = "tool_calls"
	}
	msg := map[string]any{"role": "assistant", "content": response.Content}
	if len(response.Calls) > 0 {
		msg["tool_calls"] = response.Calls
		if response.Content == "" {
			msg["content"] = nil
		}
	}
	if response.Reasoning != "" {
		msg["reasoning_content"] = response.Reasoning
	}
	raw, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), "object": "chat.completion", "created": time.Now().Unix(), "model": c.model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": response.usage()})
	if c.format != t.FormatOpenAI {
		var state any
		raw = t.TranslateNonStream(context.Background(), t.FormatOpenAI, c.format, c.model, c.req.OriginalRequest, c.req.Payload, raw, &state)
	}
	success = true
	return pluginapi.ExecutorResponse{Payload: raw, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}
func validateResult(r result) error {
	if r.Status != "COMPLETED" && r.Status != "REQUIRES_ACTION" {
		return fail(502, "Backboard did not complete the run")
	}
	if r.Status == "REQUIRES_ACTION" && len(r.Calls) == 0 {
		return fail(502, "Backboard omitted required tool calls")
	}
	seen := make(map[string]bool)
	for _, call := range r.Calls {
		var args map[string]any
		if call.ID == "" || seen[call.ID] || call.Function.Name == "" || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			return fail(502, "Invalid Backboard tool call")
		}
		seen[call.ID] = true
	}
	return nil
}
func (a *App) remember(c *completion, r result) {
	for i := range r.Calls {
		var args map[string]any
		json.Unmarshal([]byte(r.Calls[i].Function.Arguments), &args)
		raw, _ := json.Marshal(args)
		r.Calls[i].Function.Arguments = string(raw)
		r.Calls[i].Type = "function"
	}
	c.messages = append(c.messages, r.message())
	conv := c.conversation
	a.mu.Lock()
	defer a.mu.Unlock()
	conv.historyKey = hash([]any{c.scope, c.messages})
	conv.pending = r.Calls
	conv.updated = time.Now()
	conv.busy = false
	a.history[conv.historyKey] = conv
}
func (a *App) release(c *conversation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c.busy = false
	a.history[c.historyKey] = c
}
func (a *App) remove(c *conversation) bool {
	var err error
	if c.assistantID != "" {
		err = a.http(c.req, "DELETE", "/assistants/"+url.PathEscape(c.assistantID), nil, nil)
	}
	if rpcErr, ok := err.(*RPCError); ok && rpcErr.HTTPStatus == 404 {
		err = nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.history, c.historyKey)
	if err == nil {
		delete(a.conversations, c)
	} else {
		c.busy = false
		c.updated = time.Now()
	}
	return err == nil
}

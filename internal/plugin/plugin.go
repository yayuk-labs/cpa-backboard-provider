package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const ABIVersion = 1
const Version = "0.1.0"

type HostCaller func(string, any) (json.RawMessage, error)
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}
type RPCError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *RPCError) Error() string           { return e.Message }
func fail(status int, message string) error { return &RPCError{"backboard_error", message, status} }
func ErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(Envelope{Error: &RPCError{code, message, status}})
	return raw
}

type App struct {
	mu            sync.Mutex
	baseURL       string
	call          HostCaller
	closed        bool
	workers       sync.WaitGroup
	shutdown      sync.Once
	history       map[string]*conversation
	conversations map[*conversation]bool
	streams       map[string]string
}

func NewApp() *App {
	return &App{baseURL: "https://app.backboard.io/api", history: make(map[string]*conversation), conversations: make(map[*conversation]bool), streams: make(map[string]string)}
}
func (a *App) SetHostCaller(call HostCaller) { a.mu.Lock(); defer a.mu.Unlock(); a.call = call }
func (a *App) host(method string, payload, out any) error {
	a.mu.Lock()
	call := a.call
	a.mu.Unlock()
	if call == nil {
		return fail(503, "CPA host callbacks are unavailable")
	}
	raw, err := call(method, payload)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type request struct {
	Provider, AuthProvider, AuthID, FileName, Model, SourceFormat, StreamID, HostCallbackID string
	Format                                                                                  string
	Method, Path                                                                            string
	OriginalRequest                                                                         []byte
	RawJSON, StorageJSON, Payload, ConfigYAML                                               []byte
	Metadata, AuthMetadata                                                                  map[string]any
	baseURL                                                                                 string
}

func (a *App) HandleMethod(method string, raw []byte) ([]byte, error) {
	// CPA mixes SDK PascalCase fields and snake_case bridge fields.
	var fields map[string]json.RawMessage
	if len(raw) > 0 && json.Unmarshal(raw, &fields) != nil {
		return ErrorEnvelope("invalid_request", "Invalid plugin request JSON", 400), nil
	}
	normalized := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		normalized[strings.ReplaceAll(key, "_", "")] = value
	}
	data, _ := json.Marshal(normalized)
	var req request
	if json.Unmarshal(data, &req) != nil {
		return ErrorEnvelope("invalid_request", "Invalid plugin request fields", 400), nil
	}
	result, err := a.handle(method, req)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			return ErrorEnvelope(rpcErr.Code, rpcErr.Message, rpcErr.HTTPStatus), nil
		}
		return ErrorEnvelope("plugin_error", "CPA host operation failed", 502), nil
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: payload})
}

func (a *App) handle(method string, req request) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if len(req.ConfigYAML) > 0 {
			var cfg struct {
				BaseURL string `yaml:"base_url"`
			}
			if yaml.Unmarshal(req.ConfigYAML, &cfg) != nil {
				return nil, fail(400, "Invalid Backboard plugin configuration")
			}
			if cfg.BaseURL != "" {
				u, err := url.Parse(cfg.BaseURL)
				if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "127.0.0.1")) {
					return nil, fail(400, "base_url must use HTTPS (HTTP allowed for loopback tests)")
				}
				a.mu.Lock()
				a.baseURL = strings.TrimRight(cfg.BaseURL, "/")
				a.mu.Unlock()
			}
		}
		return map[string]any{"schema_version": 1, "metadata": pluginapi.Metadata{Name: "Backboard", Version: Version, Author: "naufal", GitHubRepository: "https://github.com/yayuk-labs/cpa-backboard-provider"}, "capabilities": map[string]any{"auth_provider": true, "management_api": true, "model_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions", "claude", "openai-response"}}}, nil
	case "management.register":
		return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{{Method: "POST", Path: "/plugins/backboard/cleanup", Description: "Delete idle plugin-owned Backboard assistants and threads"}}}, nil
	case "management.handle":
		if req.Method != "POST" || req.Path != "/v0/management/plugins/backboard/cleanup" {
			return pluginapi.ManagementResponse{StatusCode: 404}, nil
		}
		a.mu.Lock()
		var idle []*conversation
		for c := range a.conversations {
			if !c.busy {
				c.busy = true
				delete(a.history, c.historyKey)
				idle = append(idle, c)
			}
		}
		a.mu.Unlock()
		deleted := 0
		for _, c := range idle {
			if a.remove(c) {
				deleted++
			}
		}
		a.mu.Lock()
		remaining := len(a.conversations)
		a.mu.Unlock()
		body, _ := json.Marshal(map[string]int{"deleted": deleted, "remaining": remaining})
		return pluginapi.ManagementResponse{StatusCode: 200, Body: body}, nil
	case "auth.identifier", "executor.identifier":
		return map[string]string{"identifier": "backboard"}, nil
	case "auth.parse":
		var data map[string]any
		if json.Unmarshal(req.RawJSON, &data) != nil || data == nil {
			return nil, fail(400, "Invalid auth file JSON")
		}
		if req.Provider != "backboard" && text(data, "type") != "backboard" && text(data, "provider") != "backboard" {
			return pluginapi.AuthParseResponse{}, nil
		}
		if _, err := apiKey(req.RawJSON, data); err != nil {
			return nil, err
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: authData(req.FileName, req.RawJSON, data)}, nil
	case "auth.refresh":
		data := req.Metadata
		if len(req.StorageJSON) > 0 {
			if json.Unmarshal(req.StorageJSON, &data) != nil {
				return nil, fail(400, "Invalid auth file JSON")
			}
		}
		if _, err := apiKey(req.StorageJSON, data); err != nil {
			return nil, err
		}
		auth := authData(req.AuthID, req.StorageJSON, data)
		return pluginapi.AuthRefreshResponse{Auth: auth}, nil
	case "auth.login.start", "auth.login.poll":
		return nil, fail(501, "Import a Backboard auth file containing api_key; browser login is not supported")
	case "model.static":
		return pluginapi.ModelResponse{Provider: "backboard", Models: []pluginapi.ModelInfo{}}, nil
	case "model.for_auth":
		return a.models(req)
	case "executor.execute", "executor.execute_stream":
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return nil, fail(503, "Backboard plugin is shutting down")
		}
		a.workers.Add(1)
		a.mu.Unlock()
		defer a.workers.Done()
		if method == "executor.execute_stream" {
			return a.executeStream(req)
		}
		return a.execute(req)
	case "executor.count_tokens", "executor.http_request":
		return nil, fail(501, "This operation is not supported by the Backboard provider")
	case "plugin.quiesce", "plugin.shutdown":
		a.Shutdown()
		return struct{}{}, nil
	default:
		return nil, fail(400, fmt.Sprintf("Unknown plugin method: %s", method))
	}
}

func text(data map[string]any, key string) string { value, _ := data[key].(string); return value }
func apiKey(raw []byte, data map[string]any) (string, error) {
	if len(raw) > 0 {
		if json.Unmarshal(raw, &data) != nil {
			return "", fail(400, "Invalid Backboard credential JSON")
		}
	}
	key := text(data, "api_key")
	if key == "" || strings.ContainsFunc(key, func(r rune) bool { return r <= 32 || r == 127 }) {
		return "", fail(400, "Backboard api_key must be a nonempty string without whitespace or control characters")
	}
	return key, nil
}
func authData(filename string, raw []byte, data map[string]any) pluginapi.AuthData {
	if filename == "" {
		filename = "backboard.json"
	}
	label := text(data, "email")
	if label == "" {
		label = text(data, "label")
	}
	if label == "" {
		label = "Backboard"
	}
	disabled, _ := data["disabled"].(bool)
	return pluginapi.AuthData{Provider: "backboard", ID: filename, FileName: filename, Label: label, Prefix: text(data, "prefix"), ProxyURL: text(data, "proxy_url"), Disabled: disabled, StorageJSON: raw, Metadata: data}
}
func (a *App) Shutdown() {
	a.shutdown.Do(func() {
		a.mu.Lock()
		a.closed = true
		streams := make(map[string]string, len(a.streams))
		for up, down := range a.streams {
			streams[up] = down
		}
		a.mu.Unlock()
		for up, down := range streams {
			a.host("host.http.stream_close", map[string]string{"stream_id": up}, nil)
			a.host("host.stream.close", map[string]string{"stream_id": down}, nil)
		}
		a.workers.Wait()
		a.mu.Lock()
		entries := make([]*conversation, 0, len(a.conversations))
		for c := range a.conversations {
			entries = append(entries, c)
		}
		a.mu.Unlock()
		for _, c := range entries {
			a.remove(c)
		}
	})
}

package plugin_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-backboard-provider/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestCompletionReusesOnlyMatchingCredentialAndHistory(t *testing.T) {
	var mu sync.Mutex
	threads := 0
	deleted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "DELETE":
			deleted++
			io.WriteString(w, `{}`)
		case r.URL.Path == "/assistants":
			threads++
			fmt.Fprintf(w, `{"assistant_id":"a%d"}`, threads)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			fmt.Fprintf(w, `{"thread_id":"t%d"}`, threads)
		case r.URL.Path == "/threads/messages":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["memory"] != "off" || body["llm_provider"] != "openai" || body["model_name"] != "gpt-test" || body["system_prompt"] != "Be concise" {
				t.Errorf("wrong native payload: %v", body)
			}
			content := body["content"].(string)
			if content == "next" && body["thread_id"] != "t1" {
				t.Errorf("history not reused: %v", body)
			}
			io.WriteString(w, `{"status":"COMPLETED","content":"Hello","input_tokens":4,"cached_input_tokens":3,"cache_write_input_tokens":2,"output_tokens":2}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	app := testApp(t, server.URL)
	run := func(key, payload string) map[string]any {
		var response pluginapi.ExecutorResponse
		rpc(t, app, "executor.execute", pluginapi.ExecutorRequest{Model: "backboard/openai/gpt-test", StorageJSON: []byte(`{"api_key":"` + key + `"}`), Payload: []byte(payload), SourceFormat: "chat-completions"}, &response)
		var out map[string]any
		json.Unmarshal(response.Payload, &out)
		return out
	}
	first := `{"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"hi"}]}`
	out := run("dummy-key", first)
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "Hello" || choice["finish_reason"] != "stop" || out["usage"].(map[string]any)["prompt_tokens"] != float64(9) {
		t.Fatalf("wrong completion: %v", out)
	}
	next := `{"messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"hi"},{"role":"assistant","content":"Hello"},{"role":"user","content":"next"}]}`
	run("dummy-key", next)
	run("other-key", first)
	mu.Lock()
	count := threads
	mu.Unlock()
	if count != 2 {
		t.Fatalf("wanted two isolated threads, got %d", count)
	}
	app.Shutdown()
	mu.Lock()
	defer mu.Unlock()
	if deleted != 2 {
		t.Fatalf("owned assistants not cleaned: %d", deleted)
	}
}

func testApp(t *testing.T, serverURL string) *plugin.App {
	return streamApp(t, serverURL, nil, nil)
}

func streamApp(t *testing.T, serverURL string, frames chan []byte, closed chan string) *plugin.App {
	t.Helper()
	app := plugin.NewApp()
	var mu sync.Mutex
	readers := make(map[string]io.ReadCloser)
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.stream.emit" || method == "host.stream.close" {
			raw, _ := json.Marshal(payload)
			var chunk struct {
				Payload []byte
				Error   string
			}
			json.Unmarshal(raw, &chunk)
			if method == "host.stream.emit" {
				frames <- chunk.Payload
			} else {
				closed <- chunk.Error
			}
			return json.RawMessage(`{}`), nil
		}
		if method == "host.http.stream_read" || method == "host.http.stream_close" {
			id := payload.(map[string]string)["stream_id"]
			mu.Lock()
			reader := readers[id]
			mu.Unlock()
			if reader == nil {
				return nil, fmt.Errorf("stream closed")
			}
			if method == "host.http.stream_close" {
				reader.Close()
				mu.Lock()
				delete(readers, id)
				mu.Unlock()
				return json.RawMessage(`{}`), nil
			}
			buffer := make([]byte, 7)
			n, err := reader.Read(buffer)
			return json.Marshal(map[string]any{"payload": buffer[:n], "done": err == io.EOF})
		}
		if method != "host.http.do" && method != "host.http.do_stream" {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		raw, _ := json.Marshal(payload)
		var req pluginapi.HTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		r, err := http.NewRequest(req.Method, req.URL, strings.NewReader(string(req.Body)))
		if err != nil {
			return nil, err
		}
		r.Header = req.Headers
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			return nil, err
		}
		if method == "host.http.do_stream" {
			mu.Lock()
			id := fmt.Sprintf("upstream-%d", len(readers)+1)
			readers[id] = response.Body
			mu.Unlock()
			return json.Marshal(map[string]any{"status_code": response.StatusCode, "headers": response.Header, "stream_id": id})
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: response.StatusCode, Headers: response.Header, Body: body})
	})
	rpc(t, app, "plugin.register", map[string]any{"config_yaml": []byte("base_url: " + serverURL)}, nil)
	t.Cleanup(app.Shutdown)
	return app
}

func TestModelDiscoveryPaginatesAndKeepsProviderNamespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("X-API-Key") != "dummy-key" || r.URL.Query().Get("model_type") != "llm" {
			t.Errorf("unexpected catalog request: %s", r.URL)
			w.WriteHeader(400)
			return
		}
		if r.URL.Query().Get("skip") == "0" {
			io.WriteString(w, `{"total":2,"models":[{"name":"gpt-test","provider":"openai","context_limit":128000,"max_output_tokens":4096,"supports_tools":true}]}`)
		} else {
			io.WriteString(w, `{"total":2,"models":[{"name":"org/model","provider":"openrouter"}]}`)
		}
	}))
	defer server.Close()
	app := testApp(t, server.URL)
	var result pluginapi.ModelResponse
	rpc(t, app, "model.for_auth", pluginapi.AuthModelRequest{StorageJSON: []byte(`{"type":"backboard","api_key":"dummy-key"}`)}, &result)
	if result.Provider != "backboard" || len(result.Models) != 2 || result.Models[0].ID != "backboard/openai/gpt-test" || result.Models[0].ContextLength != 128000 || result.Models[1].ID != "backboard/openrouter/org/model" {
		t.Fatalf("incorrect catalog: %+v", result)
	}
}

func rpc(t *testing.T, app *plugin.App, method string, req, out any) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	response, err := app.HandleMethod(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope plugin.Envelope
	if err = json.Unmarshal(response, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("%s: %s", method, response)
	}
	if out != nil {
		if err = json.Unmarshal(envelope.Result, out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCredentialImportPreservesRoutingWithoutExposingKeyInLabel(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	var parsed pluginapi.AuthParseResponse
	rpc(t, app, "auth.parse", pluginapi.AuthParseRequest{Provider: "backboard", FileName: "work.json", RawJSON: []byte(`{"type":"backboard","api_key":"dummy-secret","prefix":"work","disabled":true,"note":"keep"}`)}, &parsed)
	if !parsed.Handled || parsed.Auth.Provider != "backboard" || parsed.Auth.FileName != "work.json" || parsed.Auth.Prefix != "work" || !parsed.Auth.Disabled || parsed.Auth.Metadata["note"] != "keep" || parsed.Auth.Label == "dummy-secret" {
		t.Fatalf("incorrect imported credential: %+v", parsed)
	}
	rpc(t, app, "auth.parse", pluginapi.AuthParseRequest{Provider: "codex", RawJSON: []byte(`{"type":"codex"}`)}, &parsed)
	if parsed.Handled {
		t.Fatal("claimed another provider")
	}
	for _, credential := range []string{`{"type":"backboard"}`, `{"type":"backboard","api_key":"bad\nkey"}`} {
		raw, _ := json.Marshal(map[string]any{"raw_json": []byte(credential)})
		response, _ := app.HandleMethod("auth.parse", raw)
		var env plugin.Envelope
		json.Unmarshal(response, &env)
		if env.OK || env.Error == nil || env.Error.HTTPStatus != 400 {
			t.Fatalf("invalid key accepted: %s", response)
		}
	}
}

func TestNativeToolsSubmitAllResultsOnOriginalThread(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			io.WriteString(w, `{}`)
		case r.URL.Path == "/assistants":
			io.WriteString(w, `{"assistant_id":"a-tools"}`)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			io.WriteString(w, `{"thread_id":"t-tools"}`)
		case r.URL.Path == "/threads/messages":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body["tools"].([]any)) != 1 {
				t.Error("native tools missing")
			}
			io.WriteString(w, `{"status":"REQUIRES_ACTION","tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{\"city\": \"Paris\"}"}},{"id":"call-2","type":"function","function":{"name":"weather","arguments":"{\"city\":\"London\"}"}}]}`)
		case r.URL.Path == "/threads/tool-outputs":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 3 || body["thread_id"] != "t-tools" || len(body["tool_outputs"].([]any)) != 2 {
				t.Errorf("invalid strict tool outputs: %v", body)
			}
			io.WriteString(w, `{"status":"COMPLETED","content":"Paris is sunny, London rainy","tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	app := testApp(t, server.URL)
	tools := json.RawMessage(`[{"type":"function","function":{"name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`)
	messages := []any{map[string]any{"role": "user", "content": "Check Paris and London weather"}}
	execute := func(messages []any) map[string]any {
		body, _ := json.Marshal(map[string]any{"messages": messages, "tools": tools})
		var response pluginapi.ExecutorResponse
		rpc(t, app, "executor.execute", pluginapi.ExecutorRequest{Model: "backboard/openai/gpt-test", AuthMetadata: map[string]any{"api_key": "dummy-key"}, Payload: body}, &response)
		var completion map[string]any
		json.Unmarshal(response.Payload, &completion)
		return completion
	}
	first := execute(messages)
	choice := first["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("missing tool finish: %v", first)
	}
	messages = append(messages, choice["message"], map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "sunny"}, map[string]any{"role": "tool", "tool_call_id": "call-2", "content": "rainy"})
	for _, outputs := range [][]any{
		{map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "sunny"}},
		{map[string]any{"role": "tool", "tool_call_id": "unknown", "content": "sunny"}},
		{map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "sunny"}, map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "sunny"}},
	} {
		body, _ := json.Marshal(map[string]any{"messages": append(append([]any{}, messages[:2]...), outputs...), "tools": tools})
		raw, _ := json.Marshal(pluginapi.ExecutorRequest{Model: "backboard/openai/gpt-test", AuthMetadata: map[string]any{"api_key": "dummy-key"}, Payload: body})
		response, _ := app.HandleMethod("executor.execute", raw)
		var env plugin.Envelope
		json.Unmarshal(response, &env)
		if env.Error == nil || env.Error.HTTPStatus != 400 {
			t.Fatalf("invalid tool results accepted: %s", response)
		}
	}
	second := execute(messages)
	if second["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "Paris is sunny, London rainy" {
		t.Fatalf("wrong continuation: %v", second)
	}
}

func TestStreamingEmitsFragmentedUTF8BeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			io.WriteString(w, `{}`)
		case r.URL.Path == "/assistants":
			io.WriteString(w, `{"assistant_id":"a-stream"}`)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			io.WriteString(w, `{"thread_id":"t-stream"}`)
		case r.URL.Path == "/threads/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: content_streaming\r\ndata: {\"type\":\"content_streaming\",\"content\":\"Hello 🌏\"}\r\n\r\n")
			w.(http.Flusher).Flush()
			<-release
			io.WriteString(w, "data: {\"type\":\"run_ended\",\"status\":\"completed\",\"input_tokens\":4,\"output_tokens\":2}\n\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	defer finish()
	frames := make(chan []byte, 20)
	closed := make(chan string, 2)
	app := streamApp(t, server.URL, frames, closed)
	rpc(t, app, "executor.execute_stream", map[string]any{"stream_id": "downstream", "model": "backboard/openai/gpt-test", "storage_json": []byte(`{"api_key":"dummy-key"}`), "payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}, nil)
	var output []byte
	for !bytes.Contains(output, []byte("Hello 🌏")) {
		select {
		case frame := <-frames:
			if !json.Valid(frame) {
				t.Fatalf("CPA OpenAI chunks must be raw JSON: %s", frame)
			}
			output = append(output, frame...)
		case err := <-closed:
			t.Fatalf("closed before content: %s", err)
		case <-time.After(5 * time.Second):
			t.Fatal("content buffered until terminal")
		}
	}
	finish()
	select {
	case err := <-closed:
		if err != "" {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream never closed")
	}
	for len(frames) > 0 {
		output = append(output, <-frames...)
	}
	if !bytes.Contains(output, []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("incomplete SSE: %s", output)
	}
}

func TestRegistrationProvidesRequiredCPAMetadata(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	var registered struct {
		Metadata     pluginapi.Metadata
		Capabilities map[string]any
	}
	rpc(t, app, "plugin.register", nil, &registered)
	if registered.Metadata.Name != "Backboard" || registered.Metadata.Version == "" || registered.Metadata.Author == "" || registered.Metadata.GitHubRepository == "" || registered.Capabilities["executor"] != true || registered.Capabilities["auth_provider"] != true {
		t.Fatalf("incomplete CPA registration: %+v", registered)
	}
}

func TestFailedAndTruncatedStreamsNeverReportSuccess(t *testing.T) {
	for _, events := range []string{
		"data: {\"type\":\"content_streaming\",\"content\":\"partial\"}\n\n",
		"data: {\"type\":\"error\",\"error\":\"dummy-key\"}\n\n",
		"data: {\"type\":\"run_ended\",\"status\":\"FAILED\"}\n\n",
		"data: invalid-json\n\n",
	} {
		t.Run(events, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "DELETE":
					io.WriteString(w, `{}`)
				case r.URL.Path == "/assistants":
					io.WriteString(w, `{"assistant_id":"a"}`)
				case strings.HasSuffix(r.URL.Path, "/threads"):
					io.WriteString(w, `{"thread_id":"t"}`)
				default:
					io.WriteString(w, events)
				}
			}))
			defer server.Close()
			frames := make(chan []byte, 20)
			closed := make(chan string, 2)
			app := streamApp(t, server.URL, frames, closed)
			rpc(t, app, "executor.execute_stream", map[string]any{"stream_id": "down", "model": "backboard/openai/gpt-test", "storage_json": []byte(`{"api_key":"dummy-key"}`), "payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}, nil)
			select {
			case err := <-closed:
				if err == "" || strings.Contains(err, "dummy-key") {
					t.Fatalf("unsafe stream result: %q", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed stream never closed")
			}
			for len(frames) > 0 {
				if bytes.Contains(<-frames, []byte("[DONE]")) {
					t.Fatal("failed stream reported success")
				}
			}
		})
	}
}

func TestInvalidRequestsFailBeforeCreatingUpstreamResources(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		t.Errorf("unexpected upstream call: %s", method)
		return nil, fmt.Errorf("unexpected")
	})
	for _, body := range []string{
		`{"messages":[]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`,
		`{"messages":[{"role":"assistant","content":"prefill"}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{}}]}`,
	} {
		raw, _ := json.Marshal(pluginapi.ExecutorRequest{Model: "backboard/openai/gpt-test", StorageJSON: []byte(`{"api_key":"dummy-key"}`), Payload: []byte(body)})
		response, _ := app.HandleMethod("executor.execute", raw)
		var env plugin.Envelope
		json.Unmarshal(response, &env)
		if env.Error == nil || env.Error.HTTPStatus != 400 {
			t.Fatalf("invalid request accepted: %s", response)
		}
	}
}

func TestShutdownClosesBlockedUpstreamStreams(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			io.WriteString(w, `{}`)
		case r.URL.Path == "/assistants":
			io.WriteString(w, `{"assistant_id":"a"}`)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			io.WriteString(w, `{"thread_id":"t"}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	frames := make(chan []byte, 20)
	closed := make(chan string, 3)
	app := streamApp(t, server.URL, frames, closed)
	rpc(t, app, "executor.execute_stream", map[string]any{"stream_id": "down", "model": "backboard/openai/gpt-test", "storage_json": []byte(`{"api_key":"dummy-key"}`), "payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}, nil)
	<-started
	stopped := make(chan struct{})
	go func() { app.Shutdown(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown blocked on upstream")
	}
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{Model: "backboard/openai/gpt-test", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	response, _ := app.HandleMethod("executor.execute", raw)
	var env plugin.Envelope
	json.Unmarshal(response, &env)
	if env.Error == nil || env.Error.HTTPStatus != 503 {
		t.Fatalf("execution accepted after shutdown: %s", response)
	}
}

func TestManagementCleanupDeletesOnlyIdleOwnedAssistants(t *testing.T) {
	deleted := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			deleted <- r.URL.Path
			io.WriteString(w, `{}`)
		case r.URL.Path == "/assistants":
			io.WriteString(w, `{"assistant_id":"owned"}`)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			io.WriteString(w, `{"thread_id":"owned-thread"}`)
		default:
			io.WriteString(w, `{"status":"COMPLETED","content":"Hello"}`)
		}
	}))
	defer server.Close()
	app := testApp(t, server.URL)
	rpc(t, app, "executor.execute", pluginapi.ExecutorRequest{Model: "backboard/openai/gpt-test", StorageJSON: []byte(`{"api_key":"dummy-key"}`), Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, nil)
	var response pluginapi.ManagementResponse
	rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/backboard/cleanup"}, &response)
	var result map[string]int
	json.Unmarshal(response.Body, &result)
	if response.StatusCode != 200 || result["deleted"] != 1 || result["remaining"] != 0 {
		t.Fatalf("cleanup failed: %+v %s", response, response.Body)
	}
	if path := <-deleted; path != "/assistants/owned" {
		t.Fatalf("removed unowned resource %s", path)
	}
	rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/backboard/cleanup"}, &response)
	if response.StatusCode != 404 {
		t.Fatal("cleanup accepted wrong method")
	}
}

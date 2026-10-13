package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (a *App) httpRequest(req request, method, path string, body any) (map[string]any, error) {
	metadata := req.AuthMetadata
	if len(metadata) == 0 {
		metadata = req.Metadata
	}
	key, err := apiKey(req.StorageJSON, metadata)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	base := a.baseURL
	a.mu.Unlock()
	if req.baseURL != "" {
		base = req.baseURL
	}
	return map[string]any{"method": method, "url": base + path, "headers": http.Header{"X-Api-Key": {key}, "Content-Type": {"application/json"}}, "body": raw, "host_callback_id": req.HostCallbackID}, nil
}

func (a *App) http(req request, method, path string, body, out any) error {
	payload, err := a.httpRequest(req, method, path, body)
	if err != nil {
		return err
	}
	var response pluginapi.HTTPResponse
	if err = a.host("host.http.do", payload, &response); err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return upstreamError(response.StatusCode)
	}
	if out != nil && json.Unmarshal(response.Body, out) != nil {
		return fail(502, "Invalid Backboard JSON response")
	}
	return nil
}
func upstreamError(status int) error {
	if status < 400 || status > 599 {
		status = 502
	}
	// Upstream bodies can contain user content or credentials.
	return fail(status, fmt.Sprintf("Backboard request failed (HTTP %d)", status))
}

func (a *App) models(req request) (pluginapi.ModelResponse, error) {
	result := pluginapi.ModelResponse{Provider: "backboard", Models: []pluginapi.ModelInfo{}}
	a.mu.Lock()
	configured, models := a.modelsConfigured, a.modelsConfig
	a.mu.Unlock()
	if configured {
		if _, err := apiKey(req.StorageJSON, req.Metadata); err != nil {
			return result, err
		}
		for _, m := range models {
			result.Models = append(result.Models, pluginapi.ModelInfo{ID: m.id(), Name: m.Name, Object: "model", OwnedBy: "backboard", Type: "backboard", DisplayName: m.id(), UserDefined: true, SupportedGenerationMethods: []string{"chat"}, SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}})
		}
		return result, nil
	}
	seen := make(map[string]bool)
	for skip := 0; skip < 50000; {
		var page struct {
			Total  *int `json:"total"`
			Models []struct {
				Name        string `json:"name"`
				Provider    string `json:"provider"`
				DisplayName string `json:"display_name"`
				Context     int64  `json:"context_limit"`
				Output      int64  `json:"max_output_tokens"`
				Tools       bool   `json:"supports_tools"`
			} `json:"models"`
		}
		if err := a.http(req, "GET", fmt.Sprintf("/models?model_type=llm&skip=%d&limit=500", skip), nil, &page); err != nil {
			return result, err
		}
		if page.Models == nil {
			return result, fail(502, "Invalid Backboard model list")
		}
		for _, m := range page.Models {
			if m.Name == "" || m.Provider == "" || strings.Contains(m.Provider, "/") {
				continue
			}
			id := "backboard/" + m.Provider + "/" + m.Name
			if seen[id] {
				continue
			}
			seen[id] = true
			display := m.DisplayName
			if display == "" {
				display = m.Name
			}
			info := pluginapi.ModelInfo{ID: id, Name: m.Name, Object: "model", OwnedBy: "backboard", Type: "backboard", DisplayName: display, ContextLength: m.Context, InputTokenLimit: m.Context, OutputTokenLimit: m.Output, MaxCompletionTokens: m.Output, SupportedGenerationMethods: []string{"chat"}, SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}, UserDefined: true}
			if m.Tools {
				info.SupportedParameters = []string{"tools"}
			}
			result.Models = append(result.Models, info)
		}
		skip += len(page.Models)
		if len(page.Models) == 0 || (page.Total != nil && skip >= *page.Total) || (page.Total == nil && len(page.Models) < 500) {
			return result, nil
		}
	}
	return result, fail(502, "Backboard catalog exceeds pagination limit")
}

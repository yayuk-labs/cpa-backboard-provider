package plugin

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const logoURL = "/v0/resource/plugins/cpa-backboard-provider/logo"

//go:embed assets/logo.png
var logoPNG []byte

//go:embed assets/keys.html
var keysPage []byte

func apiKeysPage() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{StatusCode: 200, Body: keysPage, Headers: http.Header{
		"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "Referrer-Policy": {"no-referrer"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
	}}
}

func managementJSON(status int, body any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(body)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: raw}
}

func (a *App) addAPIKey(req request) pluginapi.ManagementResponse {
	invalid := func(message string) pluginapi.ManagementResponse {
		return managementJSON(400, map[string]string{"error": message})
	}
	var input struct {
		APIKey string `json:"api_key"`
		Label  string `json:"label"`
		Prefix string `json:"prefix"`
	}
	if len(req.Body) > 64*1024 || json.Unmarshal(req.Body, &input) != nil {
		return invalid("Enter a valid API key request")
	}
	if _, err := apiKey(nil, map[string]any{"api_key": input.APIKey}); err != nil {
		return invalid(err.Error())
	}
	input.Label = strings.TrimSpace(input.Label)
	input.Prefix = strings.TrimSpace(input.Prefix)
	if utf8.RuneCountInString(input.Label) > 100 || strings.ContainsFunc(input.Label, func(r rune) bool { return r < 32 || r == 127 }) {
		return invalid("Label must be at most 100 characters without control characters")
	}
	if len(input.Prefix) > 64 || strings.ContainsFunc(input.Prefix, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r))
	}) {
		return invalid("Routing prefix must use letters, numbers, dots, underscores, or hyphens (up to 64 characters)")
	}
	a.addKeyMu.Lock()
	defer a.addKeyMu.Unlock()
	var files struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if a.host("host.auth.list", struct{}{}, &files) != nil {
		return managementJSON(502, map[string]string{"error": "Could not read CPA credentials; nothing was saved"})
	}
	names := make(map[string]bool)
	for _, file := range files.Files {
		names[file.Name] = true
		if file.Provider != "backboard" && file.Type != "backboard" {
			continue
		}
		var existing pluginapi.HostAuthGetResponse
		if a.host("host.auth.get", pluginapi.HostAuthGetRequest{AuthIndex: file.AuthIndex}, &existing) != nil {
			return managementJSON(502, map[string]string{"error": "Could not read an existing Backboard credential; nothing was saved"})
		}
		var data map[string]any
		if json.Unmarshal(existing.JSON, &data) != nil {
			return managementJSON(502, map[string]string{"error": "An existing Backboard credential is invalid; nothing was saved"})
		}
		if text(data, "api_key") == input.APIKey {
			return managementJSON(200, map[string]string{"status": "existing", "file_name": file.Name})
		}
	}
	name := "backboard-" + rand.Text() + ".json"
	if names[name] {
		return managementJSON(409, map[string]string{"error": "Credential filename collision; try adding the key again"})
	}
	data := map[string]string{"type": "backboard", "api_key": input.APIKey}
	if input.Label != "" {
		data["label"] = input.Label
	}
	if input.Prefix != "" {
		data["prefix"] = input.Prefix
	}
	raw, _ := json.Marshal(data)
	var catalog struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := a.http(request{StorageJSON: raw, HostCallbackID: req.HostCallbackID}, "GET", "/models?model_type=llm&limit=1", nil, &catalog); err != nil {
		message := "Could not validate the Backboard API key; nothing was saved"
		if rpcErr, ok := err.(*RPCError); ok {
			message = rpcErr.Message + "; nothing was saved"
		}
		return managementJSON(400, map[string]string{"error": message})
	}
	if catalog.Models == nil {
		return managementJSON(502, map[string]string{"error": "Backboard returned an invalid model catalog; nothing was saved"})
	}
	if a.host("host.auth.save", pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}, nil) != nil {
		return managementJSON(502, map[string]string{"error": "CPA could not confirm that the key was saved; check Auth Files before retrying"})
	}
	return managementJSON(200, map[string]string{"status": "added", "file_name": name})
}

package plugin_test

import (
	"encoding/json"
	"strings"
	"testing"

	"cpa-backboard-provider/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAddAPIKeyValidatesAndSavesSeparateCredential(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	var saved []pluginapi.HostAuthSaveRequest
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		switch method {
		case "host.auth.list":
			return json.RawMessage(`{"files":[]}`), nil
		case "host.http.do":
			var request pluginapi.HTTPRequest
			json.Unmarshal(raw, &request)
			if request.Method != "GET" || request.Headers.Get("X-API-Key") != "new-dummy-key" || !strings.HasSuffix(request.URL, "/models?model_type=llm&limit=1") {
				t.Errorf("incorrect key validation request: %+v", request)
			}
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"models":[{"name":"gpt-test","provider":"openai"}]}`)})
		case "host.auth.save":
			var request pluginapi.HostAuthSaveRequest
			json.Unmarshal(raw, &request)
			saved = append(saved, request)
			return json.Marshal(pluginapi.HostAuthSaveResponse{Name: request.Name})
		default:
			t.Errorf("unexpected callback %s", method)
			return json.RawMessage(`{}`), nil
		}
	})
	var response pluginapi.ManagementResponse
	rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/backboard/keys", Body: []byte(`{"api_key":"new-dummy-key","label":"Work","prefix":"work"}`)}, &response)
	if response.StatusCode != 200 || len(saved) != 1 || strings.Contains(string(response.Body), "new-dummy-key") {
		t.Fatalf("key not safely saved: %s", response.Body)
	}
	var auth map[string]any
	json.Unmarshal(saved[0].JSON, &auth)
	if !strings.HasPrefix(saved[0].Name, "backboard-") || !strings.HasSuffix(saved[0].Name, ".json") || auth["type"] != "backboard" || auth["api_key"] != "new-dummy-key" || auth["prefix"] != "work" || auth["label"] != "Work" {
		t.Fatalf("incorrect saved credential: %s", saved[0].JSON)
	}
}

func TestAddingExistingKeyPreservesAllAccountSettings(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.RawMessage(`{"files":[{"name":"original.json","auth_index":"work-index","provider":"backboard"}]}`), nil
		case "host.auth.get":
			return json.RawMessage(`{"name":"original.json","json":{"type":"backboard","api_key":"existing-dummy-key","disabled":true,"prefix":"old","label":"Original","proxy_url":"http://proxy","note":"keep"}}`), nil
		default:
			t.Errorf("duplicate key caused mutation or network call: %s", method)
			return json.RawMessage(`{}`), nil
		}
	})
	var response pluginapi.ManagementResponse
	rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/backboard/keys", Body: []byte(`{"api_key":"existing-dummy-key","prefix":"new","label":"Changed"}`)}, &response)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), `"status":"existing"`) || !strings.Contains(string(response.Body), "original.json") {
		t.Fatalf("duplicate not preserved: %s", response.Body)
	}
}

func TestInvalidKeyRequestsNeverSaveCredentials(t *testing.T) {
	for _, body := range []string{
		`{"api_key":"bad\nkey"}`, `{"api_key":""}`, `{"api_key":"dummy-key","prefix":"bad/prefix"}`, `{"api_key":"dummy-key","label":"bad\u0000label"}`, `invalid-json`,
	} {
		app := plugin.NewApp()
		app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
			t.Errorf("invalid input made host call: %s", method)
			return json.RawMessage(`{}`), nil
		})
		var response pluginapi.ManagementResponse
		rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/backboard/keys", Body: []byte(body)}, &response)
		if response.StatusCode != 400 {
			t.Fatalf("invalid input accepted: %s", body)
		}
		app.Shutdown()
	}
}

func TestRejectedBackboardKeyNeverSavedOrReflected(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.RawMessage(`{"files":[]}`), nil
		case "host.http.do":
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":"rejected-dummy-key"}`)})
		default:
			t.Errorf("rejected key made a write: %s", method)
			return json.RawMessage(`{}`), nil
		}
	})
	var response pluginapi.ManagementResponse
	rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/backboard/keys", Body: []byte(`{"api_key":"rejected-dummy-key"}`)}, &response)
	if response.StatusCode != 400 || strings.Contains(string(response.Body), "rejected-dummy-key") {
		t.Fatalf("unsafe rejection: %s", response.Body)
	}
}

func TestAPIKeyMenuServesPublicFormWithoutCredentials(t *testing.T) {
	app := plugin.NewApp()
	defer app.Shutdown()
	var registration pluginapi.ManagementRegistrationResponse
	rpc(t, app, "management.register", nil, &registration)
	found := false
	for _, resource := range registration.Resources {
		if resource.Path == "/keys" && resource.Menu == "Backboard API keys" {
			found = true
		}
	}
	if !found {
		t.Fatal("standalone API key menu missing")
	}
	var response pluginapi.ManagementResponse
	rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/cpa-backboard-provider/keys"}, &response)
	if response.StatusCode != 200 || response.Headers.Get("Cache-Control") != "no-store" || response.Headers.Get("Content-Security-Policy") == "" || !strings.Contains(string(response.Body), "Add API key") || !strings.Contains(string(response.Body), `type="password"`) {
		t.Fatalf("unsafe or missing key page: %d", response.StatusCode)
	}
}

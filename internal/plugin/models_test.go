package plugin_test

import (
	"encoding/json"
	"testing"

	"cpa-backboard-provider/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestConfiguredModelsSkipDiscovery(t *testing.T) {
	app := plugin.NewApp()
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		t.Fatal("catalog discovery must be bypassed")
		return nil, nil
	})
	rpc(t, app, "plugin.register", map[string]any{"config_yaml": []byte("models:\n  - provider: openai\n    name: gpt-test\n    alias: quick\n  - provider: openrouter\n    name: org/model\n")}, nil)
	var models pluginapi.ModelResponse
	rpc(t, app, "model.for_auth", map[string]any{"storage_json": []byte(`{"api_key":"dummy-key"}`)}, &models)
	if len(models.Models) != 2 || models.Models[0].ID != "quick" || models.Models[1].ID != "backboard/openrouter/org/model" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestAllowlistRejectsUnlistedRequestsAndInvalidReconfiguration(t *testing.T) {
	app := plugin.NewApp()
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		t.Fatal("unlisted model must not contact upstream")
		return nil, nil
	})
	rpc(t, app, "plugin.register", map[string]any{"config_yaml": []byte("models:\n- provider: openai\n  name: gpt-test\n  alias: quick\n")}, nil)
	for _, cfg := range []string{"models:\n- provider: openai\n  name: '*'\n", "models:\n- provider: openai\n  name: one\n  alias: same\n- provider: openai\n  name: two\n  alias: same\n"} {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(cfg)})
		response, _ := app.HandleMethod("plugin.reconfigure", raw)
		var e plugin.Envelope
		json.Unmarshal(response, &e)
		if e.OK || e.Error == nil || e.Error.HTTPStatus != 400 {
			t.Fatalf("invalid config accepted: %s", response)
		}
	}
	var models pluginapi.ModelResponse
	rpc(t, app, "model.for_auth", map[string]any{"storage_json": []byte(`{"api_key":"dummy-key"}`)}, &models)
	if len(models.Models) != 1 || models.Models[0].ID != "quick" {
		t.Fatal("failed config replaced allowlist")
	}
	reject := func(model string) {
		raw, _ := json.Marshal(pluginapi.ExecutorRequest{Model: model, StorageJSON: []byte(`{"api_key":"dummy-key"}`), Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)})
		response, _ := app.HandleMethod("executor.execute", raw)
		var e plugin.Envelope
		json.Unmarshal(response, &e)
		if e.OK || e.Error == nil || e.Error.HTTPStatus != 400 {
			t.Fatalf("unlisted model accepted: %s", response)
		}
	}
	reject("backboard/openai/other")
	rpc(t, app, "plugin.reconfigure", map[string]any{"config_yaml": []byte("models: []\n")}, nil)
	rpc(t, app, "model.for_auth", map[string]any{"storage_json": []byte(`{"api_key":"dummy-key"}`)}, &models)
	if len(models.Models) != 0 {
		t.Fatal("empty allowlist exposed models")
	}
	reject("quick")
}

func TestModelEditorValidatesBeforeSettingsAreSaved(t *testing.T) {
	app := plugin.NewApp()
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"models":[{"provider":"openai","name":"gpt-test","alias":"quick"}]}`, 200},
		{`{"models":[]}`, 200},
		{`{"models":null}`, 200},
		{`{}`, 400},
		{`{"models":[{"provider":"openai","name":"*"}]}`, 400},
		{`{"models":[{"provider":"openai","name":"one","alias":"same"},{"provider":"openai","name":"two","alias":"same"}]}`, 400},
	} {
		var response pluginapi.ManagementResponse
		rpc(t, app, "management.handle", pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/backboard/models/validate", Body: []byte(test.body)}, &response)
		if response.StatusCode != test.status {
			t.Fatalf("%s: got %d want %d", test.body, response.StatusCode, test.status)
		}
	}
}

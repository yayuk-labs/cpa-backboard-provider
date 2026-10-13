# CPA Backboard Provider

Backboard.io provider for CLIProxyAPI (CPA), packaged as a C-ABI shared-library plugin. No CPA source changes or separate bbd2api server required.

Supports API-key auth files, paginated model discovery, text completions, incremental streaming, native function tools, multi-tool continuation, and usage reporting. CPA translates incoming requests to Chat Completions; the plugin uses CPA's public translator SDK to produce OpenAI Chat Completions, Anthropic Messages, and Responses output.

## Build

Requires Go 1.26+, CGO, and a native C compiler. From this directory:

```sh
# macOS
CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared \
  -o dist/cpa-backboard-provider.dylib ./cmd/cpa-backboard-provider

# Linux
CGO_ENABLED=1 go build -tags cshared -buildmode=c-shared \
  -o dist/cpa-backboard-provider.so ./cmd/cpa-backboard-provider
```

Windows requires a matching MinGW C compiler:

```powershell
$env:CGO_ENABLED = "1"
go build -tags cshared -buildmode=c-shared -o dist/cpa-backboard-provider.dll ./cmd/cpa-backboard-provider
```

Tagged releases build and test Linux amd64 and arm64 on Ubuntu 22.04, then publish ZIP packages and SHA-256 checksums for CPA's Plugin Store. macOS ARM64 has also been built and checked locally.

## Install and authenticate

For remote Linux CPA, add the registry under `plugins.store-sources`, then choose **Backboard** in the Plugin Store and install version **0.1.3**:

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/yayuk-labs/cpa-backboard-provider/main/registry.json"
  configs:
    cpa-backboard-provider:
      enabled: true
```

[GitHub releases](https://github.com/yayuk-labs/cpa-backboard-provider/releases) contain Linux amd64/arm64 packages. CPA selects the matching architecture and verifies `checksums.txt` during installation. Restart CPA if its management interface requests it, then add the auth file below.

For manual installation:

Copy the compiled library to CPA's plugin directory. The generated header is not needed. Use a CPA build with plugin support and merge [config.example.yaml](config.example.yaml) into its configuration:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    cpa-backboard-provider:
      enabled: true
```

### Select models and aliases (v0.1.2+)

Open **Backboard**, enter your CPA management key, and click **Load model settings**. Choose **Only selected models**, then add provider/model-name rows and optional aliases. **Save model settings** validates the list and updates only `models`, preserving other plugin settings. The list applies to every Backboard key. Remove all rows to disable models, or choose **Full Backboard catalog** to restore discovery. Settings persist in CPA's configuration and reload automatically.

Set an exact allowlist under the plugin settings. CPA registers these models for every Backboard credential without fetching the full catalog:

```yaml
plugins:
  configs:
    cpa-backboard-provider:
      enabled: true
      models:
        - provider: openai
          name: gpt-4o-mini
          alias: backboard-mini
        - provider: anthropic
          name: claude-sonnet-4-6
          alias: backboard-sonnet
        - provider: openrouter
          name: anthropic/claude-sonnet-4.6
```

Request `backboard-mini` or `backboard-sonnet`. Without an alias, the public ID is `backboard/<provider>/<name>`. Credential routing prefixes still apply (e.g. `first/backboard-mini`). Canonical IDs for selected models are also accepted by the plugin. All other native models are rejected before upstream calls.

Omitting `models` keeps full catalog discovery for existing configurations. `models: []` disables all models. Entries require exact Backboard provider/name values; aliases and canonical IDs must be unique. CPA reloads saved settings automatically; model lists may take a moment to refresh. No catalog lookup validates configured names or capabilities; Backboard checks availability when requested. Existing global CPA exclusions/aliases can still hide or rename registered entries.

The sidebar entry is **Backboard**, using the embedded Backboard logo. The Plugin Store uses the same supplied image from this repository.

### Add keys from CPA (v0.1.1+)

Open **Backboard** in CPA's plugin menu, or visit `/v0/resource/plugins/cpa-backboard-provider/keys` on your CPA server. Enter your CPA management key and Backboard API key; label and routing prefix are optional. Click **Add API key**. Repeat to add more keys. Backboard validates each new key before CPA saves it in its auth directory with mode 0600.

The page keeps the management key only while open and clears the Backboard key after saving. It uses no browser storage. Duplicate keys leave the existing credential and all its settings unchanged; adding a key again does not rename, enable, or alter its routing prefix.

Use this standalone menu, rather than **OAuth Login**. Backboard uses API keys, and CPA's current plugin SDK lists credential parsers under OAuth even when browser login is unsupported. A plugin cannot insert a native AI Providers card or independently hide its OAuth card through this SDK.

For API clients, use the authenticated management route:

```sh
curl -X POST http://127.0.0.1:8317/v0/management/plugins/backboard/keys \
  -H 'Authorization: Bearer MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"api_key":"YOUR_BACKBOARD_API_KEY","label":"Work","prefix":"work"}'
```

### Import an auth file

Create `backboard-account.json` in CPA's configured `oauth.auth-dir`, with restrictive file permissions:

```json
{
  "type": "backboard",
  "api_key": "YOUR_BACKBOARD_API_KEY",
  "label": "Personal Backboard"
}
```

```sh
chmod 600 /path/to/auths/backboard-account.json
```

Existing `prefix`, `disabled`, `proxy_url`, and other metadata are preserved. The key is never used as an account label or included in plugin error messages. API keys do not require OAuth refresh; browser login is unsupported. Restart CPA after installation.

## Request a model

Read CPA's `/v1/models` for exact IDs. IDs include the Backboard catalog's provider, including gateway model names containing `/`:

- `backboard/openai/gpt-4o-mini`
- `backboard/anthropic/claude-sonnet-4-6`
- `backboard/openrouter/<catalog-model-name>`

Catalog availability does not guarantee the account can execute every model.

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer CPA_CLIENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"backboard/openai/gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Anthropic clients can use the same model IDs with `/v1/messages`:

```sh
curl http://127.0.0.1:8317/v1/messages \
  -H 'Authorization: Bearer CPA_CLIENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"backboard/openai/gpt-4o-mini","max_tokens":128,"messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

## Conversations and tools

Threads are scoped to the upstream base URL, API key, model, system prompt, tool definitions, and complete conversation history. Reused threads receive only the new user message. Simultaneous requests reserve separate threads rather than mutating the same thread concurrently.

Cold conversation history is sent as a labeled JSON transcript: Backboard's documented API does not import individual assistant/tool roles. System instructions remain in `system_prompt`; no dynamic tags or instructions are stripped.

Tools use Backboard's native OpenAI function schemas. Return all pending results together with their original call IDs. Streaming emits incremental text/reasoning; authoritative tool calls arrive when Backboard requests tool submission.

Native tool continuation requires the original in-memory thread, full history, and unchanged credential/model/system/tools. Missing or expired continuations return HTTP 409. Unknown, duplicate, or incomplete tool results return HTTP 400 without consuming the pending run. When using several Backboard keys in CPA, use an auth routing prefix to pin a tool conversation to one credential.

Each cold conversation creates a plugin-owned assistant/thread. Cache capacity is 256 conversations; idle entries expire after 30 minutes and are removed on subsequent requests. Plugin shutdown attempts to remove owned assistants and their threads, but current CPA unload revokes host HTTP callbacks before shutdown. For reliable cleanup, call the authenticated cleanup route while the plugin is running, before disabling it or stopping CPA. Failed deletions are retained for retry while the plugin runs; abrupt process termination or persistent deletion failures can leave resources in Backboard. Existing user assistants are never deleted. Memory is explicitly disabled.

```sh
curl -X POST http://127.0.0.1:8317/v0/management/plugins/backboard/cleanup \
  -H 'Authorization: Bearer MANAGEMENT_KEY'
```

This removes idle plugin-owned conversations, including pending tool runs. Active requests are skipped. The response reports `deleted` and `remaining`; repeat after active requests finish if needed. CPA's current plugin-route SDK mounts management routes under `/v0/management`, even on v8 hosts.

## Compatibility limits

- Text content and function tools only; image/audio/file content and assistant prefills return HTTP 400.
- `tool_choice` supports `auto` and `none`; forced/named choices return HTTP 400.
- Backboard's documented message endpoint does not accept OpenAI output-token limits, sampling parameters, or stop sequences. These fields are not forwarded, including Anthropic's required `max_tokens`; model/provider defaults apply.
- JSON output settings and reasoning controls are not forwarded. Token counting and arbitrary authenticated HTTP forwarding return HTTP 501. No quota-reset capability is advertised.
- Host HTTP callbacks use CPA's proxy and cancellation policy. CPA preserves stored per-account `proxy_url`, but the callback transport uses the host's global proxy settings.
- Conversation caches are local to one plugin instance; they are not persisted or shared between CPA hosts.

## Verification

```sh
go test -race ./...
go vet ./...
gofmt -l .
python3 scripts/smoke.py --cpa /path/to/cli-proxy-api \
  --plugin dist/cpa-backboard-provider.dylib
```

The smoke check runs an isolated CPA and fake Backboard server. It verifies model registration, downstream OpenAI/Anthropic completions and streams, usage, and native tool continuations. RPC tests cover credential isolation, malformed requests/results, fragmented UTF-8, failed/truncated streams, and shutdown cancellation.

Verified on macOS ARM64: race tests, vet (including the C-shared build), compiled-plugin CPA smoke checks for OpenAI/Anthropic/Responses, and live Backboard checks with `gpt-4o-mini` for OpenAI/Anthropic completions and streams plus native tool continuation. The live catalog registered 16,338 models during verification; availability can change.

For limited live checks, export `BBD_API_KEY` in your shell and run:

```sh
python3 scripts/smoke.py --live --cpa /path/to/cli-proxy-api \
  --plugin dist/cpa-backboard-provider.dylib
```

Live checks incur model usage. Credentials are stored only in the temporary isolated auth directory (mode 0600), removed when the check exits. Default test model: `backboard/openai/gpt-4o-mini`; override with `--model`.

## References and license

- [Backboard messages and stream events](https://docs.backboard.io/concepts/messages)
- [Backboard native tool calling](https://docs.backboard.io/concepts/tool-calling)
- [Backboard model catalog](https://docs.backboard.io/concepts/models)
- [CPA plugin examples](https://github.com/router-for-me/CLIProxyAPI/tree/main/examples/plugin)
- Local `cpa-zed-provider` supplied the C-ABI bridge pattern; its MIT notice is retained in [LICENSE](LICENSE).
- Local `bbd2api` was reviewed as a behavioral reference. Its source was not copied; this provider implements the documented Backboard API independently.

To publish a new version, update `Version` in `internal/plugin/plugin.go` and `version` in `registry.json`, commit, then push a matching `v<version>` tag. The release workflow tests both Linux architectures, builds the shared libraries, and publishes the store packages and checksums.

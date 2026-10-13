#!/usr/bin/env python3
"""Verify the shared library through CPA; optionally use BBD_API_KEY for live checks."""
import argparse
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


class Backboard(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, body):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        assert self.path == "/models?model_type=llm&limit=1", self.path
        if self.headers.get("X-API-Key") not in {"dummy-key", "dummy-key-2"}:
            self.send_response(401)
            self.end_headers()
            return
        self.reply({"total": 1, "models": [{"name": "gpt-test", "provider": "openai", "context_limit": 128000, "supports_tools": True}]})

    def do_DELETE(self):
        assert self.path.startswith("/assistants/"), self.path
        self.reply({})

    def do_POST(self):
        assert self.headers["X-API-Key"] in {"dummy-key", "dummy-key-2"}
        size = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(size) or b"{}")
        if self.path == "/assistants":
            return self.reply({"assistant_id": str(time.time_ns())})
        if self.path.endswith("/threads"):
            return self.reply({"thread_id": str(time.time_ns())})
        assert self.path in ("/threads/messages", "/threads/tool-outputs"), self.path
        if self.path == "/threads/tool-outputs":
            assert set(body) == {"thread_id", "tool_outputs", "stream"}, body
            assert body["tool_outputs"][0]["output"] == "sunny", body
            content = "Paris is sunny"
            calls = []
        else:
            assert body["memory"] == "off" and body["llm_provider"] == "openai" and body["model_name"] == "gpt-test", body
            content = "Hello"
            calls = []
            if body.get("tools"):
                content = ""
                calls = [{"id": "call-weather", "type": "function", "function": {"name": "weather", "arguments": '{"city":"Paris"}'}}]
        result = {"status": "REQUIRES_ACTION" if calls else "COMPLETED", "content": content, "tool_calls": calls, "input_tokens": 4, "output_tokens": 2}
        if self.path=="/threads/tool-outputs":
            result["tool_calls"]=[{"id":"call-weather","type":"function","function":{"name":"weather","arguments":'{"city":"Paris"}'}}]
        if not body["stream"]:
            return self.reply(result)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        events = []
        if content:
            events.append({"type": "content_streaming", "content": content})
        events.append(dict(result, type="tool_submit_required" if calls else "run_ended"))
        for event in events:
            self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
            self.wfile.flush()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--model", default="backboard/openai/gpt-4o-mini")
    args = parser.parse_args()
    key = os.environ.get("BBD_API_KEY") if args.live else "dummy-key"
    if not key:
        parser.error("--live requires BBD_API_KEY")
    upstream = None
    if not args.live:
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), Backboard)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
    model = args.model if args.live else "first/quick"
    try:
        with tempfile.TemporaryDirectory(prefix="cpa-backboard-smoke-") as directory:
            root = Path(directory)
            (root / "plugins").mkdir()
            (root / "auths").mkdir(mode=0o700)
            shutil.copy2(args.plugin, root / "plugins" / args.plugin.name)
            auth = root / "auths" / "backboard.json"
            auth.touch(mode=0o600)
            auth.write_text(json.dumps({"type": "backboard", "api_key": key, **({} if args.live else {"prefix":"first"})}))
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 0))
                port = probe.getsockname()[1]
            base_config = "" if args.live else f'      base_url: "http://127.0.0.1:{upstream.server_port}"\n'
            native_provider, native_name = (args.model.removeprefix("backboard/").split("/", 1) if args.live else ("openai", "gpt-test"))
            base_config += f'      models:\n        - provider: {json.dumps(native_provider)}\n          name: {json.dumps(native_name)}\n'
            if not args.live:
                base_config += '          alias: quick\n'
            config = root / "config.yaml"
            config.write_text(f'''config-version: 8
server:
  host: "127.0.0.1"
  port: {port}
management:
  secret-key: "smoke-management"
  disable-control-panel: true
access:
  api-keys: ["smoke-client"]
oauth:
  auth-dir: {json.dumps(str(root / "auths"))}
plugins:
  enabled: true
  dir: {json.dumps(str(root / "plugins"))}
  configs:
    cpa-backboard-provider:
      enabled: true
{base_config}''')
            with (root / "cpa.log").open("w+") as log:
                process = subprocess.Popen([str(args.cpa.resolve()), "-config", str(config), "-local-model"], cwd=root, stdout=log, stderr=log)
                try:
                    def call(path, body=None, client_key="smoke-client"):
                        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, data=None if body is None else json.dumps(body).encode(), headers={"Authorization": "Bearer "+client_key, "Content-Type": "application/json"})
                        try:
                            with urllib.request.urlopen(request, timeout=90) as response:
                                return response.read()
                        except urllib.error.HTTPError as error:
                            detail = error.read().decode().replace(key, "[redacted]")
                            raise RuntimeError(f"{path}: HTTP {error.code}: {detail}") from None

                    deadline = time.monotonic() + 120
                    while True:
                        try:
                            catalog = json.loads(call("/v1/models"))["data"]
                            if any(item["id"] == model for item in catalog):
                                break
                        except (OSError, ValueError, RuntimeError):
                            pass
                        if process.poll() is not None or time.monotonic() > deadline:
                            raise RuntimeError("CPA did not register the Backboard model")
                        time.sleep(0.5)
                    print(f"Plugin loaded; {len(catalog)} models registered", flush=True)
                    if not args.live:
                        page_url = f"http://127.0.0.1:{port}/v0/resource/plugins/cpa-backboard-provider/keys"
                        with urllib.request.urlopen(page_url, timeout=10) as response:
                            page = response.read()
                            assert b"Backboard API keys" in page and b"Add API key" in page, page
                            assert response.headers["Cache-Control"] == "no-store"
                        plugins = json.loads(call("/v8/management/plugins", client_key="smoke-management"))["plugins"]
                        backboard = next(p for p in plugins if p["id"]=="cpa-backboard-provider")
                        assert len(backboard["menus"]) == 1, backboard
                        assert any(menu["path"].endswith("/keys") and menu["menu"] == "Backboard" for menu in backboard["menus"]), backboard
                        assert backboard["logo"] == "/v0/resource/plugins/cpa-backboard-provider/logo", backboard
                        with urllib.request.urlopen(f"http://127.0.0.1:{port}" + backboard["logo"], timeout=10) as response:
                            assert response.headers["Content-Type"] == "image/png"
                            assert response.read().startswith(b"\x89PNG\r\n\x1a\n")
                        try:
                            call("/v0/management/plugins/backboard/keys", {"api_key":"dummy-key-2"}, client_key="smoke-client")
                        except RuntimeError as error:
                            assert "HTTP 401" in str(error) or "HTTP 403" in str(error), error
                        else:
                            raise AssertionError("Key entry accepted a client key without management access")
                        existing = json.loads(call("/v0/management/plugins/backboard/keys", {"api_key":"dummy-key", "label":"Do not overwrite"}, client_key="smoke-management"))
                        assert existing["status"]=="existing", existing
                        added = json.loads(call("/v0/management/plugins/backboard/keys", {"api_key":"dummy-key-2", "label":"Second", "prefix":"second"}, client_key="smoke-management"))
                        assert added["status"]=="added", added
                        stored = json.loads((root/"auths"/added["file_name"]).read_text())
                        assert stored["api_key"]=="dummy-key-2" and stored["prefix"]=="second", stored
                        assert (root/"auths"/added["file_name"]).stat().st_mode & 0o777 == 0o600
                        try:
                            call("/v0/management/plugins/backboard/keys", {"api_key":"invalid-key"}, client_key="smoke-management")
                        except RuntimeError as error:
                            assert "HTTP 400" in str(error), error
                        else:
                            raise AssertionError("Rejected upstream key was saved")
                        print("Standalone key page + authenticated add + preservation OK",flush=True)
                    try:
                        call("/v0/management/plugins/backboard/cleanup", {}, client_key="smoke-client")
                    except RuntimeError as error:
                        assert "HTTP 401" in str(error) or "HTTP 403" in str(error), error
                    else:
                        raise AssertionError("Cleanup route accepted a client key without management access")
                    messages = [{"role": "user", "content": "Reply with exactly Hello"}]
                    request = {"model": model, "messages": messages}
                    response = json.loads(call("/v1/chat/completions", request))
                    assert "Hello" in response["choices"][0]["message"]["content"], response
                    assert response["usage"]["total_tokens"] > 0, response
                    print("OpenAI completion + usage OK", flush=True)
                    stream = call("/v1/chat/completions", dict(request, stream=True))
                    assert b"Hello" in stream and b"data: [DONE]" in stream, stream
                    for line in stream.splitlines():
                        if line.startswith(b"data:") and line[5:].strip()!=b"[DONE]":
                            json.loads(line[5:].strip())
                    print("OpenAI streaming OK", flush=True)
                    claude = json.loads(call("/v1/messages", dict(request, max_tokens=128)))
                    assert "Hello" in claude["content"][0]["text"], claude
                    claude_stream = call("/v1/messages", dict(request, stream=True, max_tokens=128))
                    assert b"Hello" in claude_stream and b"message_stop" in claude_stream, claude_stream
                    print("Anthropic completion + streaming OK", flush=True)
                    for line in claude_stream.splitlines():
                        if line.startswith(b"data:"):
                            json.loads(line[5:].strip())
                    if not args.live:
                        responses_request = {"model": model, "input": "Hello"}
                        responses = json.loads(call("/v1/responses", responses_request))
                        assert "Hello" in json.dumps(responses), responses
                        responses_stream = call("/v1/responses", dict(responses_request, stream=True))
                        assert b"response.completed" in responses_stream and b"Hello" in responses_stream, responses_stream
                        for line in responses_stream.splitlines():
                            if line.startswith(b"data:"):
                                json.loads(line[5:].strip())
                        print("Responses completion + streaming OK", flush=True)
                    tools = [{"type": "function", "function": {"name": "weather", "description": "Get weather for a city", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}]
                    tool_messages = [{"role": "user", "content": "Use the weather tool to get Paris weather. Do not guess."}]
                    first = json.loads(call("/v1/chat/completions", {"model": model, "messages": tool_messages, "tools": tools}))
                    assistant = first["choices"][0]["message"]
                    assert first["choices"][0]["finish_reason"] == "tool_calls", first
                    outputs = [{"role": "tool", "tool_call_id": tc["id"], "content": "sunny"} for tc in assistant["tool_calls"]]
                    second = json.loads(call("/v1/chat/completions", {"model": model, "messages": tool_messages + [assistant] + outputs, "tools": tools}))
                    assert "sunny" in second["choices"][0]["message"]["content"].lower(), second
                    print("Native tool calling + continuation OK", flush=True)
                    if not args.live:
                        stream = call("/v1/chat/completions", {"model": model, "messages": tool_messages, "tools": tools, "stream": True})
                        assert b"weather" in stream and b'"tool_calls"' in stream and b"[DONE]" in stream, stream
                        tool_chunks = [json.loads(line[5:].strip()) for line in stream.splitlines() if line.startswith(b"data:") and line[5:].strip()!=b"[DONE]"]
                        calls = next(chunk["choices"][0]["delta"]["tool_calls"] for chunk in tool_chunks if chunk["choices"][0]["delta"].get("tool_calls"))
                        calls = [{name: value for name, value in tc.items() if name!="index"} for tc in calls]
                        outputs = [{"role":"tool","tool_call_id":tc["id"],"content":"sunny"} for tc in calls]
                        continued = call("/v1/chat/completions", {"model":model,"messages":tool_messages+[{"role":"assistant","content":None,"tool_calls":calls}]+outputs,"tools":tools,"stream":True})
                        assert b"sunny" in continued and b"[DONE]" in continued, continued
                        anthropic_tools = [{"name": "weather", "description": "Get weather", "input_schema": tools[0]["function"]["parameters"]}]
                        first = json.loads(call("/v1/messages", {"model": model, "max_tokens": 128, "messages": tool_messages, "tools": anthropic_tools}))
                        assert first["stop_reason"] == "tool_use", first
                        tool = next(block for block in first["content"] if block["type"] == "tool_use")
                        continuation = tool_messages + [{"role": "assistant", "content": first["content"]}, {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tool["id"], "content": "sunny"}]}]
                        second = json.loads(call("/v1/messages", {"model": model, "max_tokens": 128, "messages": continuation, "tools": anthropic_tools}))
                        assert "sunny" in json.dumps(second), second
                        print("Anthropic native tools + continuation OK", flush=True)
                except Exception:
                    log.flush()
                    log.seek(0)
                    print(log.read().replace(key, "[redacted]")[-8000:])
                    raise
                finally:
                    try:
                        cleaned=json.loads(call("/v0/management/plugins/backboard/cleanup", {}, client_key="smoke-management"))
                        assert cleaned["remaining"]==0, cleaned
                        print("Plugin-owned resources cleaned",flush=True)
                    finally:
                        process.terminate()
                        try:
                            process.wait(timeout=30)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait()
    finally:
        if upstream:
            upstream.shutdown()
            upstream.server_close()


if __name__ == "__main__":
    main()

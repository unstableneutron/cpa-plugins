#!/usr/bin/env python3
"""Exercise separately built Go libraries together through a real CPA server.

Supply a reviewed host executable and a directory containing plugin libraries.
No downloads, installation into a live service, or live provider calls occur.
"""

import argparse
import concurrent.futures
import http.client
import json
import os
import platform
import shutil
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


# Deliberately absent from the fallback catalog: discovery must actually work.
MODEL = "fixture/native-e2e"
MODEL_X = "fixture/qualification-x"
MODEL_Y = "fixture/qualification-y"
MODEL_STREAM = "fixture/qualification-stream"
FRONTEND_KEY = "fixture-frontend"
ACCOUNT_A_KEY = "fixture-commandcode-a"
ACCOUNT_B_KEY = "fixture-commandcode-b"
qualification_failure = "unsupported_model"
seen = []
seen_lock = threading.Lock()
cancel_started = threading.Event()
cancel_observed = threading.Event()


class Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def reply(self, status, body, content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path != "/provider/v1/models":
            self.reply(404, b"{}")
            return
        self.reply(200, json.dumps({"data": [
            {"id": model, "name": "Fixture", "context_length": 1000000}
            for model in (MODEL, MODEL_X, MODEL_Y, MODEL_STREAM)
        ]}).encode())

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with seen_lock:
            seen.append((self.path, dict(self.headers), body))
        if self.path != "/alpha/generate":
            self.reply(404, b"{}")
            return
        request_model = json.loads(body)["params"]["model"]
        authorization = self.headers.get("Authorization")
        if request_model == MODEL_STREAM:
            self.reply(200, (
                'data: {"type":"text-delta","text":"partial-denial"}\n'
                'data: {"type":"error","statusCode":400,"error":{"code":"unsupported_model","message":"not offered"}}\n'
            ).encode(), "application/x-ndjson")
            return
        if authorization == f"Bearer {ACCOUNT_A_KEY}" and request_model == MODEL_X:
            if qualification_failure == "upgrade_required":
                self.reply(403, b'{"error":{"code":"upgrade_required","type":"permission_error","message":"upgrade"}}')
            else:
                self.reply(400, b'{"error":{"code":"unsupported_model","type":"invalid_request_error","message":"not offered"}}')
            return
        if authorization == f"Bearer {ACCOUNT_B_KEY}" and request_model == MODEL_Y:
            self.reply(400, b'{"error":{"code":"unsupported_model","type":"invalid_request_error","message":"not offered"}}')
            return
        if b"fixture-cancel-stream" in body:
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b'data: {"type":"text-delta","text":"cancel-ready"}\n')
            self.wfile.flush()
            cancel_started.set()
            self.connection.settimeout(15)
            try:
                if self.connection.recv(1) == b"":
                    cancel_observed.set()
            except ConnectionResetError:
                cancel_observed.set()
            finally:
                self.close_connection = True
            return
        self.reply(200, (
            'data: {"type":"text-delta","text":"native-ok"}\n'
            'data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}\n'
        ).encode(), "application/x-ndjson")

    def do_PATCH(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with seen_lock:
            seen.append((self.path, dict(self.headers), body))
        self.reply(207, body, "application/octet-stream")

    def log_message(self, *_args):
        pass


def request(port, method, path, body=None, headers=None):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
    try:
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        return response.status, response.read()
    finally:
        connection.close()


def qualify(host, plugins, environment_auth=False, failure_code="unsupported_model"):
    global qualification_failure
    qualification_failure = failure_code
    system = {"Linux": "linux", "Darwin": "darwin", "Windows": "windows"}[platform.system()]
    arch = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    extension = {"linux": ".so", "darwin": ".dylib", "windows": ".dll"}[system]
    with tempfile.TemporaryDirectory(prefix="cpa-native-e2e-") as temporary:
        root = Path(temporary)
        library_dir = root / "plugins" / system / arch
        library_dir.mkdir(parents=True)
        for library in plugins.glob("*" + extension):
            shutil.copy2(library, library_dir / library.name)
        for name in ("commandcode", "chatgpt-backend"):
            assert (library_dir / (name + extension)).exists(), f"missing {name} library"
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        probe = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        port = probe.server_address[1]
        probe.server_close()
        base = f"http://127.0.0.1:{upstream.server_address[1]}"
        auths = root / "auths"
        auths.mkdir()
        if not environment_auth:
            for name, key in (("a", ACCOUNT_A_KEY), ("b", ACCOUNT_B_KEY)):
                (auths / f"commandcode-{name}.json").write_text(json.dumps({
                    "type": "commandcode", "api_key": key, "base_url": base,
                }))
        (auths / "codex.json").write_text(json.dumps({
            "type": "codex", "access_token": "fixture-stored-token", "account_id": "acct-123",
        }))
        environment_config = f'''  api-keys:
    - provider: commandcode
      api-key-env: CPA_FIXTURE_KEY_A
      base-url: "{base}"
    - provider: commandcode
      api-key-env: CPA_FIXTURE_KEY_B
      base-url: "{base}"
''' if environment_auth else ""
        config = root / "config.yaml"
        config.write_text(f'''host: "127.0.0.1"
port: {port}
auth-dir: "{auths}"
api-keys: [{FRONTEND_KEY}]
remote-management:
  disable-control-panel: true
plugins:
  enabled: true
  dir: "{root / 'plugins'}"
{environment_config}  configs:
    commandcode:
      enabled: true
    chatgpt-backend:
      enabled: true
      base-url: "{base}"
''')
        try:
            with (root / "host.log").open("w+") as log:
                process = subprocess.Popen([str(host), "--config", str(config), "--local-model"], cwd=root, stdout=log, stderr=subprocess.STDOUT,
                                           env={**os.environ, "COMMAND_CODE_API_KEY": "fixture-dummy-canonical", "COMMANDCODE_API_KEY": "fixture-dummy-legacy",
                                                "CPA_FIXTURE_KEY_A": ACCOUNT_A_KEY, "CPA_FIXTURE_KEY_B": ACCOUNT_B_KEY})
                try:
                    deadline = time.monotonic() + 30
                    while True:
                        assert process.poll() is None, "host exited before readiness"
                        try:
                            status, models = request(port, "GET", "/v1/models", headers={"Authorization": f"Bearer {FRONTEND_KEY}"})
                            initialized = b"core auth auto-refresh started" in (root / "host.log").read_bytes()
                            if status == 200 and MODEL.encode() in models and initialized:
                                break
                        except OSError:
                            pass
                        assert time.monotonic() < deadline, "CommandCode model not registered"
                        time.sleep(0.1)
                    headers = {"Authorization": f"Bearer {FRONTEND_KEY}", "Content-Type": "application/json"}

                    def completion(stream):
                        body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": "hello"}], "stream": stream})
                        status, response = request(port, "POST", "/v1/chat/completions", body, headers)
                        assert status == 200, (status, response)
                        if stream:
                            assert b"native-ok" in response and b"[DONE]" in response, response
                        else:
                            result = json.loads(response)
                            assert result["choices"][0]["message"]["content"] == "native-ok", result
                            assert result["usage"]["total_tokens"] == 3, result

                    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                        list(pool.map(completion, [False, True] * 8))
                    for endpoint, terminal, fields in (
                        ("/v1/responses", b"response.completed", {"input": "hello"}),
                        ("/v1/messages", b"message_stop", {"messages": [{"role": "user", "content": "hello"}], "max_tokens": 32}),
                    ):
                        for stream in (False, True):
                            body = json.dumps({"model": MODEL, "stream": stream, **fields})
                            status, response = request(port, "POST", endpoint, body, headers)
                            assert status == 200 and b"native-ok" in response, (endpoint, status, response)
                            if stream:
                                assert terminal in response, response
                            else:
                                result = json.loads(response)
                                assert result["usage"]["input_tokens"] == 2, result
                                assert result["usage"]["output_tokens"] == 1, result

                    def qualification_request(model, stream=False):
                        body = json.dumps({"model": model, "stream": stream, "messages": [{"role": "user", "content": "qualification"}]})
                        status, response = request(port, "POST", "/v1/chat/completions", body, headers)
                        assert status == 200 and b"native-ok" in response, (model, status, response)

                    qualification_request(MODEL_X)
                    qualification_request(MODEL_X)
                    qualification_request(MODEL_Y, stream=True)
                    qualification_request(MODEL_Y)
                    for expected_attempts in (1, 2):
                        body = json.dumps({"model": MODEL_STREAM, "stream": True, "messages": [{"role": "user", "content": "qualification"}]})
                        status, response = request(port, "POST", "/v1/chat/completions", body, headers)
                        assert status == 200 and b"partial-denial" in response and b'"error"' in response, (status, response)
                        attempts = [item for item in seen if item[0] == "/alpha/generate" and json.loads(item[2])["params"]["model"] == MODEL_STREAM]
                        assert len(attempts) == expected_attempts, "stream replayed after content"
                    assert attempts[0][1]["Authorization"] != attempts[1][1]["Authorization"], "stream denial did not cool account/model"
                    path = "/backend-api/files/a%2Fb?cursor=a%2Bb&cursor=a+b"
                    payload = bytes(range(256)) * 4096
                    backend_headers = {"Authorization": f"Bearer {FRONTEND_KEY}", "ChatGPT-Account-ID": "acct-123"}
                    status, response = request(port, "PATCH", path, payload, backend_headers)
                    assert status == 207 and response == payload, (status, len(response))
                    count = len(seen)
                    status, _ = request(port, "PATCH", path, b"denied", {"Authorization": "Bearer invalid-fixture-key", "ChatGPT-Account-ID": "acct-123"})
                    assert status in (401, 403) and len(seen) == count, status
                    command_requests = [item for item in seen if item[0] == "/alpha/generate"]
                    qualification_attempts = {}
                    for _, request_headers, body in command_requests:
                        parsed = json.loads(body)
                        if parsed["params"]["model"] in (MODEL_X, MODEL_Y):
                            key = (request_headers["Authorization"], parsed["params"]["model"])
                            qualification_attempts[key] = qualification_attempts.get(key, 0) + 1
                    assert qualification_attempts[(f"Bearer {ACCOUNT_A_KEY}", MODEL_X)] == 1, qualification_attempts
                    assert qualification_attempts[(f"Bearer {ACCOUNT_B_KEY}", MODEL_X)] == 2, qualification_attempts
                    assert qualification_attempts[(f"Bearer {ACCOUNT_B_KEY}", MODEL_Y)] == 1, qualification_attempts
                    assert qualification_attempts[(f"Bearer {ACCOUNT_A_KEY}", MODEL_Y)] == 2, qualification_attempts
                    for _, request_headers, body in command_requests:
                        normalized = {key.lower(): value for key, value in request_headers.items()}
                        assert normalized["authorization"] in (f"Bearer {ACCOUNT_A_KEY}", f"Bearer {ACCOUNT_B_KEY}")
                        assert normalized["x-command-code-version"] == "1.15.0"
                        assert normalized["x-session-id"] == json.loads(body)["threadId"]
                    backend_request = [item for item in seen if item[0] == path]
                    assert len(backend_request) == 1 and backend_request[0][2] == payload
                    normalized = {key.lower(): value for key, value in backend_request[0][1].items()}
                    assert normalized["authorization"] == "Bearer fixture-stored-token"
                    assert normalized["chatgpt-account-id"] == "acct-123"
                    # Keep upstream open, then disconnect downstream. The native
                    # stream must cancel the host HTTP reader instead of leaking it.
                    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
                    connection.request("POST", "/v1/chat/completions", json.dumps({
                        "model": MODEL, "stream": True,
                        "messages": [{"role": "user", "content": "fixture-cancel-stream"}],
                    }), headers)
                    response = connection.getresponse()
                    try:
                        assert response.status == 200
                        assert cancel_started.wait(5), "upstream stream did not start"
                        assert b"cancel-ready" in response.readline()
                    finally:
                        response.close()
                        connection.close()
                    assert cancel_observed.wait(5), "downstream disconnect did not cancel upstream"
                except Exception:
                    log.flush()
                    log.seek(0)
                    diagnostics = log.read()
                    for secret in (FRONTEND_KEY, ACCOUNT_A_KEY, ACCOUNT_B_KEY, "fixture-stored-token", "fixture-dummy-canonical", "fixture-dummy-legacy"):
                        diagnostics = diagnostics.replace(secret, "[REDACTED]")
                    print("\n".join(diagnostics.splitlines()[:60]))
                    print("Fixture request paths:", [item[0] for item in seen])
                    raise
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=20)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                    assert process.returncode == 0, f"host shutdown status {process.returncode}"
                    if environment_auth:
                        assert not list(auths.glob("commandcode-*.json"))
                        for path in auths.rglob("*"):
                            if path.is_file():
                                assert ACCOUNT_A_KEY.encode() not in path.read_bytes() and ACCOUNT_B_KEY.encode() not in path.read_bytes(), "environment key was persisted"
        finally:
            upstream.shutdown()
            upstream.server_close()
            thread.join()
    print("PASS multi-library load, catalog/auth, 16 concurrent stream/nonstream calls, exact usage, backend binary/path/query/credential preservation, frontend auth, stream cancellation, graceful shutdown")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", type=Path, required=True)
    parser.add_argument("--plugins", type=Path, required=True)
    parser.add_argument("--environment-auth", action="store_true")
    parser.add_argument("--qualification-failure", choices=("unsupported_model", "upgrade_required"), default="unsupported_model")
    args = parser.parse_args()
    qualify(args.host.resolve(), args.plugins.resolve(), args.environment_auth, args.qualification_failure)

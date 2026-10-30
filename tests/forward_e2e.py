#!/usr/bin/env python3
"""Qualify embeddings-forward through a real CPA host and local upstream fixtures.

Supply a reviewed host executable (plugin schema 9 or later) and a directory
containing the embeddings-forward library. No live provider calls occur.
"""

import argparse
import http.client
import json
import platform
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

FRONTEND_KEY = "fixture-frontend-key"
KEYS = {name: f"fixture-{name}-key" for name in ("openrouter", "commandcode", "tokenrouter", "explabs")}
REFUSED = "please refuse this question"
seen = []
seen_lock = threading.Lock()


def answers(questions, extra=""):
    body = ",".join(f'{json.dumps(key)}:{{"type":"noul","noul":0.5}}' for key in questions)
    return f'{{"model":"jev-1.14.0","answers":{{{body}}},"usage":{{"input_tokens":{len(questions)},"output_tokens":1}}{extra}}}'


def embeddings_body(count, dims):
    rows = ",".join('{"object":"embedding","index":%d,"embedding":[%s]}' % (i, ",".join("%.9f" % ((i * dims + d) / 3e7 - 0.01) for d in range(dims))) for i in range(count))
    return '{"object":"list","data":[%s],"model":"text-embedding-3-small","note":"<b>&amp;\'","usage":{"prompt_tokens":%d}}' % (rows, count)


EMBEDDINGS_SMALL = embeddings_body(2, 4)
EMBEDDINGS_LARGE = embeddings_body(2048, 256)
SYSTEMONE_PASSTHROUGH = '{"id":"gen-dec-9","model":"typesafe/jev-1.14-20261014","provider":"TypeSafe","answers":{"billing":{"type":"noul","noul":0.97}},"usage":{"cost":0.00002,"input_tokens":12,"output_tokens":3}}'


class Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def reply(self, status, body, content_type="application/json"):
        payload = body.encode() if isinstance(body, str) else body
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        body = json.loads(raw)
        with seen_lock:
            seen.append((self.path, self.headers.get("Authorization"), body))
        model = body.get("model")
        questions = list(body.get("questions", {}))
        if self.path == "/explabs/v1/embeddings":
            self.reply(429, '{"error":{"message":"insufficient credits"}}')
        elif self.path == "/tokenrouter/v1/embeddings":
            self.reply(200, EMBEDDINGS_LARGE if len(body["input"]) == 2048 else EMBEDDINGS_SMALL)
        elif self.path == "/openrouter/api/v1/rerank":
            self.reply(503, "upstream overloaded", "text/plain")
        elif self.path == "/tokenrouter/v1/rerank":
            self.reply(200, '{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}]}')
        elif self.path == "/openrouter/api/alpha/decisions" and model == "typesafe/jev-1.14":
            self.reply(402, '{"error":{"code":402,"message":"billing_error"}}')
        elif self.path == "/openrouter/api/alpha/decisions":
            if len(questions) > 200:
                self.reply(400, '{"error":{"message":"too many questions"}}')
            elif REFUSED.encode() in raw:
                self.reply(502, "error code: 502", "text/plain")
            else:
                self.reply(200, answers(questions, ',"id":"gen-dec-1","provider":"OpenAI"'))
        elif self.path == "/commandcode/provider/v1/systemone":
            ok = len(questions) <= 100 and not ({"provider", "session_id", "trace", "user"} & set(body))
            self.reply(200 if ok else 400, answers(questions) if ok else '{"detail":"invalid"}')
        elif self.path == "/tokenrouter/api/alpha/decisions":
            self.reply(200, SYSTEMONE_PASSTHROUGH)
        else:
            self.reply(404, '{"error":{"message":"fixture route not found"}}')

    def log_message(self, *_args):
        pass


def request(port, path, body=None, headers=None, method="POST"):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=60)
    try:
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        return response.status, response.read()
    finally:
        connection.close()


def config_text(root, port, base):
    providers = {name: f"{base}/{name}" for name in KEYS}
    return f'''host: "127.0.0.1"
port: {port}
auth-dir: "{root / 'auths'}"
api-keys: [{FRONTEND_KEY}]
remote-management:
  disable-control-panel: true
plugins:
  enabled: true
  dir: "{root / 'plugins'}"
  configs:
    embeddings-forward:
      enabled: true
      embeddings:
        enabled: true
        providers:
          - name: explabs
            base_url: {providers["explabs"]}/v1
            api_keys: [{KEYS["explabs"]}]
            cooldown_seconds: 60
            models: [{{name: text-embedding-3-small, alias: openai/text-embedding-3-small}}]
          - name: tokenrouter
            base_url: {providers["tokenrouter"]}/v1
            api_keys: [{KEYS["tokenrouter"]}]
            models: [{{name: openai/text-embedding-3-small}}, {{name: text-embedding-3-small}}]
      rerank:
        enabled: true
        providers:
          - name: openrouter
            base_url: {providers["openrouter"]}/api/v1
            api_keys: [{KEYS["openrouter"]}]
            models: [{{name: cohere/rerank-v3.5}}]
          - name: tokenrouter
            base_url: {providers["tokenrouter"]}/v1
            api_keys: [{KEYS["tokenrouter"]}]
            models: [{{name: cohere/rerank-v3.5}}]
      decisions:
        enabled: true
        providers:
          - name: openrouter
            base_url: {providers["openrouter"]}/api
            api_keys: [{KEYS["openrouter"]}]
            max_questions: 200
            cooldown_seconds: 600
            models:
              - {{name: openai/gpt-6-luna-decisions, alias: luna}}
              - {{name: typesafe/jev-1.14, alias: jev-latest}}
          - name: commandcode
            base_url: {providers["commandcode"]}/provider
            api: systemone
            api_keys: [{KEYS["commandcode"]}]
            max_questions: 100
            models: [{{name: typesafe/jev, alias: luna}}]
          - name: tokenrouter
            base_url: {providers["tokenrouter"]}/api
            api_keys: [{KEYS["tokenrouter"]}]
            models: [{{name: typesafe/jev-1.14, alias: jev-latest}}]
'''


def qualify(host, plugins, runner=()):
    system = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    extension = {"linux": ".so", "darwin": ".dylib"}[system]
    with tempfile.TemporaryDirectory(prefix="cpa-forward-e2e-") as temporary:
        root = Path(temporary)
        library_dir = root / "plugins" / system / arch
        library_dir.mkdir(parents=True)
        (root / "auths").mkdir()
        shutil.copy2(plugins / ("embeddings-forward" + extension), library_dir)
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        probe = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        port = probe.server_address[1]
        probe.server_close()
        config = root / "config.yaml"
        config.write_text(config_text(root, port, f"http://127.0.0.1:{upstream.server_address[1]}"))
        auth = {"Authorization": f"Bearer {FRONTEND_KEY}", "Content-Type": "application/json"}
        log = (root / "host.log").open("w+")
        process = subprocess.Popen([*runner, str(host), "--config", str(config), "--local-model"], cwd=root, stdout=log, stderr=subprocess.STDOUT)
        try:
            small = json.dumps({"model": "openai/text-embedding-3-small", "input": ["a", "b"], "dimensions": 4})
            deadline = time.monotonic() + 30
            while True:
                assert process.poll() is None, "host exited before readiness"
                try:
                    status, body = request(port, "/v1/embeddings", small, auth)
                    if status == 200:
                        break
                except OSError:
                    pass
                assert time.monotonic() < deadline, "embeddings route not ready"
                time.sleep(0.1)
            # Byte-identical: no HTML sanitizing, alias rewritten only for explabs.
            assert body == EMBEDDINGS_SMALL.encode(), body[:200]
            embedded = [item for item in seen if item[0].endswith("/embeddings")]
            assert embedded[0][0] == "/explabs/v1/embeddings" and embedded[0][2]["model"] == "text-embedding-3-small", embedded[0]
            assert embedded[1][2]["model"] == "openai/text-embedding-3-small" and embedded[1][1] == f"Bearer {KEYS['tokenrouter']}"

            count = len(seen)
            for headers in ({"Content-Type": "application/json"}, {"Authorization": "Bearer wrong-key"}):
                status, body = request(port, "/v1/embeddings", small, headers)
                assert status == 401 and len(seen) == count, (status, body)

            # explabs is cooling now, so the large batch goes straight to TokenRouter.
            large = json.dumps({"model": "text-embedding-3-small", "input": ["token " * 64 + str(i) for i in range(2048)]})
            status, body = request(port, "/v1/embeddings", large, auth)
            assert status == 200 and body == EMBEDDINGS_LARGE.encode(), (status, len(body))
            assert seen[-1][0] == "/tokenrouter/v1/embeddings" and len(seen) == count + 1

            status, body = request(port, "/v1/rerank", json.dumps({"model": "cohere/rerank-v3.5", "query": "q", "documents": ["a", "b"]}), auth)
            assert status == 200 and json.loads(body)["results"][0]["index"] == 1, (status, body)

            questions = {f"q{i:03d}": {"type": "noul", "instructions": REFUSED if i == 7 else f"Is memory {i} relevant?"} for i in range(250)}
            decisions = {"model": "luna", "state": {"query": "<recall>"}, "questions": questions, "session_id": "s1"}
            status, body = request(port, "/api/alpha/decisions", json.dumps(decisions), auth)
            assert status == 200, (status, body[:300])
            merged = json.loads(body)
            assert list(merged["answers"]) == list(questions), "answers missing or reordered"
            assert merged["usage"]["input_tokens"] == 250, merged["usage"]
            batches = [len(item[2]["questions"]) for item in seen if "decisions" in item[0] or "systemone" in item[0]]
            assert sorted(batches) == [50, 100, 100, 200], batches

            systemone = {"model": "jev-latest", "state": "I was charged twice.", "questions": {"billing": {"type": "noul", "instructions": "Is this about billing?"}}}
            for _ in range(2):
                status, body = request(port, "/v1/systemone", json.dumps(systemone), auth)
                assert status == 200 and body == SYSTEMONE_PASSTHROUGH.encode(), (status, body)
            billing = [item for item in seen if item[2].get("model") == "typesafe/jev-1.14" and item[0].startswith("/openrouter")]
            assert len(billing) == 1, "402 provider was not cooled"

            # The host rejects a declared oversized body before reading it.
            with socket.create_connection(("127.0.0.1", port), timeout=10) as oversized:
                oversized.sendall(f"POST /v1/rerank HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer {FRONTEND_KEY}\r\n"
                                  f"Content-Type: application/json\r\nContent-Length: {33 << 20}\r\n\r\n{{".encode())
                assert oversized.recv(64).startswith(b"HTTP/1.1 413"), "oversized body was not rejected"
            status, body = request(port, "/v1/embeddings", json.dumps({"model": "unknown", "input": "x"}), auth)
            assert status == 404 and json.loads(body)["error"]["code"] == "model_not_found", body
            status, _ = request(port, "/v1/models", headers=auth, method="GET")
            assert status == 200, "built-in route was shadowed"
        except Exception:
            log.flush()
            log.seek(0)
            text = log.read()
            for secret in [FRONTEND_KEY, *KEYS.values()]:
                text = text.replace(secret, "[REDACTED]")
            print("\n".join(text.splitlines()[-60:]))
            raise
        finally:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            upstream.shutdown()
            upstream.server_close()
        log.flush()
        log.seek(0)
        text = log.read()
        assert process.returncode == 0, f"host shutdown status {process.returncode}"
        assert "ingress handler completed" in text, "host did not serve handler routes"
        for secret in [FRONTEND_KEY, *KEYS.values()]:
            assert secret not in text, "host log contains a credential"
        log.close()
    print("PASS client-key auth, alias rewrite, 429/402/5xx failover, cooldown, batch split/merge, "
          "System One/Decisions translation, raw responses, large batch, 413, route precedence, log secrecy")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", type=Path, required=True)
    parser.add_argument("--plugins", type=Path, required=True)
    parser.add_argument("--runner", nargs=argparse.REMAINDER, default=[])
    args = parser.parse_args()
    qualify(args.host.resolve(), args.plugins.resolve(), args.runner)

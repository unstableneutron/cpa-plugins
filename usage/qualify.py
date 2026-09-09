#!/usr/bin/env python3
"""End-to-end qualification for cpa-usage-statistics v0.1.0.

The caller builds reviewed source and supplies host/library paths. This script
never downloads or executes release binaries.
"""

from __future__ import annotations

import argparse
import json
import shutil
import signal
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


CALLER_KEY = "qualification-caller-secret"
MANAGEMENT_KEY = "qualification-management-secret"
UPSTREAM_KEY = "qualification-upstream-secret"
MODEL = "fixture-model"
FAILURE_DETAIL = "qualification-private-failure-detail"


class UpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self) -> None:  # noqa: N802 - stdlib callback name
        length = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(length))
        content = request.get("messages", [{}])[0].get("content")
        if content == "fail":
            payload = json.dumps({"error": {"message": FAILURE_DETAIL}}).encode()
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        payload = json.dumps(
            {
                "id": "fixture-response",
                "object": "chat.completion",
                "model": MODEL,
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "ok"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": 11,
                    "completion_tokens": 7,
                    "total_tokens": 18,
                },
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def request_json(url: str, *, key: str | None = None, body: dict | None = None) -> tuple[int, object]:
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    if key is not None:
        request.add_header("Authorization", f"Bearer {key}")
    try:
        with urllib.request.urlopen(request, timeout=3) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        raw = error.read()
        return error.code, json.loads(raw) if raw else {}


def wait_ready(url: str, process: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError("CPA exited before becoming ready")
        try:
            with urllib.request.urlopen(url, timeout=0.5):
                return
        except urllib.error.HTTPError:
            # Any HTTP response proves that the listener and routes are ready.
            return
        except (urllib.error.URLError, TimeoutError):
            time.sleep(0.05)
    raise RuntimeError("CPA did not become ready")


def start_host(host: Path, config: Path, log: Path) -> tuple[subprocess.Popen[str], object]:
    output = log.open("a", encoding="utf-8")
    process = subprocess.Popen(
        [str(host), "--config", str(config), "--no-browser", "--local-model"],
        stdout=output,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return process, output


def stop_host(process: subprocess.Popen[str], output: object) -> None:
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
    output.close()
    if process.returncode != 0:
        raise RuntimeError(f"CPA exited with status {process.returncode}")


def find_details(payload: object) -> list[dict]:
    if not isinstance(payload, dict):
        return []
    details: list[dict] = []
    for models in payload.values():
        if not isinstance(models, dict):
            continue
        for records in models.values():
            if isinstance(records, list):
                details.extend(item for item in records if isinstance(item, dict))
    return details


def sanitized_log_tail(log: Path) -> str:
    text = log.read_text(encoding="utf-8") if log.exists() else ""
    for secret in (CALLER_KEY, MANAGEMENT_KEY, UPSTREAM_KEY):
        text = text.replace(secret, "[REDACTED]")
    return "\n".join(text.splitlines()[-120:])


def qualify(host: Path, library: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="cpa-usage-qualification-") as raw_dir:
        root = Path(raw_dir)
        plugin_dir = root / "plugins" / "linux" / "amd64"
        plugin_dir.mkdir(parents=True)
        shutil.copy2(library, plugin_dir / "usage-statistics.so")
        data_dir = root / "data"

        upstream = ThreadingHTTPServer(("127.0.0.1", 0), UpstreamHandler)
        upstream_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        upstream_thread.start()
        upstream_port = upstream.server_address[1]

        probe = ThreadingHTTPServer(("127.0.0.1", 0), BaseHTTPRequestHandler)
        host_port = probe.server_address[1]
        probe.server_close()

        config = root / "config.yaml"
        config.write_text(
            f'''host: "127.0.0.1"
port: {host_port}
remote-management:
  allow-remote: false
  secret-key: "{MANAGEMENT_KEY}"
  disable-control-panel: true
auth-dir: "{root / 'auths'}"
api-keys:
  - "{CALLER_KEY}"
plugins:
  enabled: true
  dir: "{root / 'plugins'}"
  configs:
    usage-statistics:
      enabled: true
      priority: 100
      data_dir: "{data_dir}"
      retention_days: 0
openai-compatibility:
  - name: fixture
    base-url: "http://127.0.0.1:{upstream_port}/v1"
    request-retry: 0
    api-key-entries:
      - api-key: "{UPSTREAM_KEY}"
    models:
      - name: "{MODEL}"
''',
            encoding="utf-8",
        )
        log = root / "host.log"
        base = f"http://127.0.0.1:{host_port}"
        usage_url = base + "/v0/management/plugins/usage-statistics/usage"

        process, output = start_host(host, config, log)
        try:
            wait_ready(base + "/health", process)
            status, _ = request_json(usage_url)
            if status not in (401, 403):
                raise AssertionError(f"unauthenticated management status = {status}")

            for _ in range(2):
                status, response = request_json(
                    base + "/v1/chat/completions",
                    key=CALLER_KEY,
                    body={"model": MODEL, "messages": [{"role": "user", "content": "hi"}]},
                )
                if status != 200:
                    raise AssertionError(f"proxy status = {status}, response = {response}")
            status, _ = request_json(
                base + "/v1/chat/completions",
                key=CALLER_KEY,
                body={"model": MODEL, "messages": [{"role": "user", "content": "fail"}]},
            )
            if status != 429:
                raise AssertionError(f"failure proxy status = {status}, want 429")

            deadline = time.monotonic() + 10
            details: list[dict] = []
            payload: object = {}
            while time.monotonic() < deadline:
                status, payload = request_json(usage_url, key=MANAGEMENT_KEY)
                details = find_details(payload)
                if status == 200 and len(details) == 3:
                    break
                time.sleep(0.05)
            if len(details) != 3:
                raise AssertionError(
                    f"persisted records = {len(details)}, want 3\n"
                    f"host log tail:\n{sanitized_log_tail(log)}"
                )
            totals = {
                "input": sum(item["tokens"]["input_tokens"] for item in details),
                "output": sum(item["tokens"]["output_tokens"] for item in details),
                "total": sum(item["tokens"]["total_tokens"] for item in details),
            }
            if totals != {"input": 22, "output": 14, "total": 36}:
                raise AssertionError(f"token totals = {totals}")
            failures = [item for item in details if item["failed"]]
            if len(failures) != 1 or failures[0].get("failure_status_code") != 429:
                raise AssertionError(f"failure details = {failures}")
            if FAILURE_DETAIL not in failures[0].get("failure_body", ""):
                raise AssertionError("failure body was not persisted")
            if CALLER_KEY not in payload:
                raise AssertionError("v0.1.0 privacy gap changed: raw caller key was not the grouping key")
        finally:
            stop_host(process, output)

        process, output = start_host(host, config, log)
        try:
            wait_ready(base + "/health", process)
            status, restarted = request_json(usage_url, key=MANAGEMENT_KEY)
            if status != 200 or len(find_details(restarted)) != 3:
                raise AssertionError("records did not survive graceful shutdown/restart")
        finally:
            stop_host(process, output)
            upstream.shutdown()
            upstream.server_close()
            upstream_thread.join(timeout=2)

        log_text = log.read_text(encoding="utf-8")
        for secret in (CALLER_KEY, MANAGEMENT_KEY, UPSTREAM_KEY):
            if secret in log_text:
                raise AssertionError(
                    "host log contains a qualification secret\n"
                    f"host log tail:\n{sanitized_log_tail(log)}"
                )
        if FAILURE_DETAIL not in log_text:
            raise AssertionError("known host failure-body log exposure changed")

    print("PASS native load, queued delivery, exact totals/failure, management auth, key redaction, graceful restart persistence")
    print("KNOWN PRIVACY GAPS v0.1.0 stores raw caller key/failure body; host warning log includes raw failure body")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", type=Path, required=True)
    parser.add_argument("--plugin", type=Path, required=True)
    args = parser.parse_args()
    if not args.host.is_file() or not args.plugin.is_file():
        parser.error("--host and --plugin must name existing files")
    qualify(args.host.resolve(), args.plugin.resolve())


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Opt-in, low-volume live qualification. Secrets and raw responses stay in memory.

Only api.commandcode.ai receives the environment key. No redirects, account
endpoint guesses, automatic retries, or raw logs are permitted.
"""
import argparse
import base64
import hashlib
import http.client
import json
import os
import re
import socket
import subprocess
import tempfile
import threading
import time
import zipfile
from pathlib import Path
from urllib.parse import quote

ORIGIN = "api.commandcode.ai"
MODEL = "deepseek/deepseek-v4-flash"


def safe_summary(status, headers, data):
    quota_headers = {}
    for name, value in headers:
        if re.search(r"rate.?limit|quota|credit|remaining|reset|retry-after", name, re.I):
            quota_headers[name.lower()] = value if re.fullmatch(r"[0-9., :TZ+/-]{1,100}", value) else "[non-numeric value omitted]"
    summary = {"status": status, "quota_headers": quota_headers}
    if isinstance(data, dict):
        summary["body_fields"] = sorted(data)
        usage = data.get("usage")
        if isinstance(usage, dict):
            summary["usage"] = {k: v for k, v in usage.items() if isinstance(v, (int, float))}
        error = data.get("error")
        if isinstance(error, dict):
            summary["error_fields"] = sorted(error)
            message = str(error.get("message", "")).lower()
            summary["error_categories"] = [name for name in (
                "no auth", "auth_not_found", "auth_unavailable", "executor",
                "upstream", "capacity", "upgrade", "permission", "overload",
                "unavailable", "connection", "timeout", "model",
            ) if name in message]
            for name in ("code", "type"):
                value = error.get(name)
                if isinstance(value, str) and re.fullmatch(r"[a-z_]{1,64}", value):
                    summary["error_" + name] = value
    return summary


def check_secret(secret, data):
    # Never echo the offending bytes, even if an upstream reflects a credential.
    variants = (secret.encode(), quote(secret, safe="").encode(), base64.b64encode(secret.encode()))
    if any(value in data for value in variants):
        raise RuntimeError("secret detected in response/log; contents withheld")


def direct(secret, path, payload=None):
    connection = http.client.HTTPSConnection(ORIGIN, timeout=20)
    try:
        connection.connect()
        connection.sock.settimeout(None)
        headers = {"Authorization": "Bearer " + secret, "Content-Type": "application/json", "x-cmd-zdr": "1"}
        connection.request("GET" if payload is None else "POST", path, None if payload is None else json.dumps(payload), headers)
        response = connection.getresponse()
        raw = response.read(2 * 1024 * 1024)
        check_secret(secret, raw)
        check_secret(secret, repr(response.getheaders()).encode())
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            data = {"non_json_response": True}
        return data, safe_summary(response.status, response.getheaders(), data)
    finally:
        connection.close()


def native(secret, host, archive, checksum):
    results = {}
    if hashlib.sha256(archive.read_bytes()).hexdigest() != checksum:
        raise RuntimeError("reviewed archive checksum mismatch")
    with tempfile.TemporaryDirectory(prefix="commandcode-live-") as temporary:
        root = Path(temporary)
        plugin_dir = root / "plugins/linux/amd64"
        plugin_dir.mkdir(parents=True)
        with zipfile.ZipFile(archive) as bundle:
            (plugin_dir / "commandcode.so").write_bytes(bundle.read("commandcode.so"))
        auth_dir = root / "auths"
        auth_dir.mkdir()
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        config = root / "config.yaml"
        config.write_text(f'''host: "127.0.0.1"
port: {port}
auth-dir: "{auth_dir}"
api-keys: [live-smoke-frontend]
commercial-mode: true
logging-to-file: false
request-log: false
usage-statistics-enabled: false
request-retry: 0
max-retry-credentials: 1
remote-management:
  disable-control-panel: true
plugins:
  enabled: true
  dir: "{root / 'plugins'}"
  api-keys:
    - provider: commandcode
      api-key-env: COMMANDCODE_API_KEY
  configs:
    commandcode:
      enabled: true
''')
        # Do not inherit production storage, logging, proxy, or other credentials.
        env = {"PATH": os.environ["PATH"], "HOME": str(root), "COMMANDCODE_API_KEY": secret}
        process = subprocess.Popen([str(host), "--config", str(config), "--local-model"], cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        logs = bytearray()
        ready = threading.Event()

        def collect_logs():
            for line in process.stdout:
                logs.extend(line)
                if b"core auth auto-refresh started" in line:
                    ready.set()

        reader = threading.Thread(target=collect_logs, daemon=True)
        reader.start()

        def request(path, payload=None, stream=False, cancel=False, authorized=True):
            connection = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
            try:
                connection.connect()
                connection.sock.settimeout(None)
                headers = {"Content-Type": "application/json", "Authorization": "Bearer " + ("live-smoke-frontend" if authorized else "invalid")}
                connection.request("GET" if payload is None else "POST", path, None if payload is None else json.dumps(payload), headers)
                response = connection.getresponse()
                status = response.status
                response_headers = response.getheaders()
                check_secret(secret, repr(response_headers).encode())
                if not stream or status != 200:
                    raw = response.read(2 * 1024 * 1024)
                    check_secret(secret, raw)
                    return json.loads(raw), safe_summary(status, response_headers, json.loads(raw))
                usage = None
                content = False
                done = False
                try:
                    for line in response:
                        check_secret(secret, line)
                        if not line.startswith(b"data:"):
                            continue
                        value = line[5:].strip()
                        if value == b"[DONE]":
                            done = True
                            break
                        chunk = json.loads(value)
                        if isinstance(chunk.get("usage"), dict):
                            usage = chunk["usage"]
                        if any(choice.get("delta", {}).get("content") for choice in chunk.get("choices", [])):
                            content = True
                            if cancel:
                                break
                finally:
                    response.close()
                return None, {**safe_summary(status, response_headers, {"usage": usage}), "content_received": content, "terminal_received": done, "client_canceled": cancel and content}
            finally:
                connection.close()

        try:
            # Static discovery becomes visible before account model registration.
            if not ready.wait(30):
                raise RuntimeError("account initialization incomplete; logs withheld")
            deadline = time.monotonic() + 30
            while True:
                if process.poll() is not None:
                    raise RuntimeError("host exited; raw logs withheld")
                try:
                    models, result = request("/v1/models")
                    if result["status"] == 200 and any(m.get("id") == MODEL for m in models.get("data", [])):
                        results["environment_auth_without_json"] = True
                        break
                except OSError:
                    pass
                if time.monotonic() >= deadline:
                    raise RuntimeError("model registration not ready; raw logs withheld")
                time.sleep(0.1)
            _, results["frontend_error"] = request("/v1/chat/completions", {"model": MODEL, "messages": []}, authorized=False)
            base = {"model": MODEL, "messages": [{"role": "user", "content": "Reply with OK only."}], "max_tokens": 64}
            body, results["nonstream"] = request("/v1/chat/completions", base)
            results["nonstream"]["content_received"] = bool(body.get("choices", [{}])[0].get("message", {}).get("content"))
            if results["nonstream"]["status"] == 200:
                _, results["stream"] = request("/v1/chat/completions", {**base, "stream": True}, stream=True)
                _, results["cancel"] = request("/v1/chat/completions", {**base, "stream": True, "messages": [{"role": "user", "content": "Count from one to twenty, one number per line."}]}, stream=True, cancel=True)
        finally:
            process.terminate()
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            reader.join()
            check_secret(secret, bytes(logs))
            for path in root.rglob("*"):
                if path.is_file() and path.suffix != ".so":
                    check_secret(secret, path.read_bytes())
            results["secret_scan"] = "clean"
            results["auth_files_created"] = len(list(auth_dir.iterdir()))
            results["shutdown_exit"] = process.returncode
    return results


def native_passed(result):
    return (
        result.get("secret_scan") == "clean"
        and result.get("auth_files_created") == 0
        and result.get("shutdown_exit") == 0
        and result.get("frontend_error", {}).get("status") == 401
        and all(result.get(mode, {}).get("status") == 200 for mode in ("nonstream", "stream", "cancel"))
        and all(result.get(mode, {}).get("content_received") for mode in ("nonstream", "stream"))
        and all(result.get(mode, {}).get("usage", {}).get("total_tokens", 0) > 0 for mode in ("nonstream", "stream"))
        and result.get("stream", {}).get("terminal_received") is True
        and result.get("cancel", {}).get("client_canceled") is True
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", type=Path)
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--sha256", default="8f199142a16de2e4c6d46ce4d422a6716b3622500dd5b8e2c6f99b51054ecdea")
    parser.add_argument("--phase", choices=("all", "documented", "native"), default="all")
    args = parser.parse_args()
    secret = os.environ.get("COMMANDCODE_API_KEY", "").strip()
    if not secret:
        raise RuntimeError("COMMANDCODE_API_KEY is not injected")
    results = {}
    if args.phase != "native":
        models, results["models"] = direct(secret, "/provider/v1/models")
        entries = models.get("data", [])
        results["models"]["count"] = len(entries)
        results["models"]["item_fields"] = sorted({key for item in entries for key in item})
        payload = {"model": MODEL, "messages": [{"role": "user", "content": "Reply with OK only."}], "max_tokens": 64}
        _, results["documented_success"] = direct(secret, "/provider/v1/chat/completions", payload)
        _, results["documented_error"] = direct(secret, "/provider/v1/chat/completions", {**payload, "model": "fixture-invalid-model"})
    if args.phase != "documented" and args.host and args.archive:
        results["native"] = native(secret, args.host.resolve(), args.archive.resolve(), args.sha256)
        results["native"]["passed"] = native_passed(results["native"])
    encoded = json.dumps(results, indent=2).encode()
    check_secret(secret, encoded)
    print(encoded.decode())
    if "native" in results and not results["native"]["passed"]:
        raise SystemExit(2)
    if "documented_success" in results and results["documented_success"]["status"] != 200:
        raise SystemExit(2)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Exception strings may contain response data or headers. Do not echo them.
        print(json.dumps({"result": "failed", "exception_type": type(error).__name__, "details": "withheld for secret safety"}))
        raise SystemExit(1)

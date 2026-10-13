#!/usr/bin/env python3
"""Smoke test an extracted native codex-proxy release binary.

Tests binary identity, help, no-credential startup, authentication enforcement,
health, fake-credential reload, locking, graceful shutdown, and log sanitation.
Never contacts upstream or uses real credentials.
"""

import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

IS_WINDOWS = sys.platform == "win32"
if not IS_WINDOWS:
    import fcntl


def request(base, path, key=None):
    headers = {"Authorization": "Bearer " + key} if key else {}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(urllib.request.Request(base + path, headers=headers), timeout=2) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, None


def wait_healthy(base):
    for _ in range(100):
        try:
            status, body = request(base, "/health")
            if status == 200 and body.get("status") == "ok":
                return body
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.05)
    raise RuntimeError("proxy did not become healthy")


def get_free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_binary(binary_path: Path):
    assert binary_path.is_file(), f"Binary not found: {binary_path}"

    # 1. Version command
    result = subprocess.run([str(binary_path), "version"], capture_output=True, text=True)
    assert result.returncode == 0, f"version failed: {result.stderr}"
    assert "codex-proxy v" in result.stdout, f"unexpected version: {result.stdout}"
    assert result.stderr == ""

    # 2. Help command
    result = subprocess.run([str(binary_path), "-help"], capture_output=True, text=True)
    assert result.returncode == 0 or result.returncode == 2  # flag.ErrHelp
    assert "Usage of" in result.stderr or "Usage of" in result.stdout or "-addr" in result.stderr

    # 3. No-credential startup and lifecycle test
    with tempfile.TemporaryDirectory() as temp_dir:
        dir_path = Path(temp_dir)
        auth_file = dir_path / "auth.json"
        history_dir = dir_path / "history"
        lock_file = dir_path / "auth.json.codex-proxy.lock"
        port = get_free_port()
        base = f"http://127.0.0.1:{port}"

        env = os.environ.copy()
        env["CODEX_PROXY_ADDR"] = f"127.0.0.1:{port}"
        env["CODEX_PROXY_API_KEY"] = "smoke-inference-key"
        env["CODEX_PROXY_MANAGEMENT_KEY"] = "smoke-mgmt-key"
        env["CODEX_PROXY_AUTH_FILE"] = str(auth_file)
        env["CODEX_PROXY_HISTORY_DIR"] = str(history_dir)
        env["CODEX_PROXY_QUOTA_POLL_INTERVAL"] = "5m"

        cmd = [str(binary_path), "serve"]
        proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            health = wait_healthy(base)
            assert health["credential_configured"] is False

            # Inference auth enforcement
            assert request(base, "/v1/models")[0] == 401
            # Valid inference key with no credentials configured returns 503
            assert request(base, "/v1/models", "smoke-inference-key")[0] == 503

            # Management auth enforcement
            assert request(base, "/status/account")[0] == 401
            assert request(base, "/status/account", "smoke-inference-key")[0] == 401
            # Valid management key with no credentials configured returns 503
            assert request(base, "/status/account", "smoke-mgmt-key")[0] == 503

            # Verify lock file is held by the running proxy on POSIX
            if not IS_WINDOWS:
                assert lock_file.exists()
                with lock_file.open("rb") as stream:
                    try:
                        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    except BlockingIOError:
                        pass
                    else:
                        raise AssertionError("Auth lock was not held while proxy was running")

            # Graceful shutdown
            if IS_WINDOWS:
                proc.terminate()
            else:
                proc.send_signal(signal.SIGTERM)

            stdout, stderr = proc.communicate(timeout=10)
            assert proc.returncode == 0, f"Shutdown returned non-zero: {proc.returncode}, stderr: {stderr}"

            # Verify lock file is released on POSIX
            if not IS_WINDOWS:
                with lock_file.open("rb") as stream:
                    fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)

            # Check logs do not expose sensitive values
            all_logs = stdout + stderr
            assert "smoke-inference-key" not in all_logs
            assert "smoke-mgmt-key" not in all_logs

        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

        # 4. Restart test with synthetic credentials
        synthetic_auth = {
            "auth_mode": "chatgpt",
            "tokens": {
                "access_token": "dummy-access-token",
                "refresh_token": "dummy-refresh-token",
                "account_id": "acc-smoke-test-12345",
            },
        }
        auth_file.write_text(json.dumps(synthetic_auth))
        auth_file.chmod(0o600)

        port = get_free_port()
        base = f"http://127.0.0.1:{port}"
        env["CODEX_PROXY_ADDR"] = f"127.0.0.1:{port}"

        proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            health = wait_healthy(base)
            assert health["credential_configured"] is True

            # Account status should return the synthetic profile
            status_code, body = request(base, "/status/account", "smoke-mgmt-key")
            assert status_code == 200, f"Expected 200, got {status_code}: {body}"
            assert body["account_id"] == "acc-smoke-test-12345"

            # Graceful shutdown
            if IS_WINDOWS:
                proc.terminate()
            else:
                proc.send_signal(signal.SIGTERM)

            stdout, stderr = proc.communicate(timeout=10)
            assert proc.returncode == 0

            # Log check: tokens must never be logged
            all_logs = stdout + stderr
            assert "dummy-access-token" not in all_logs
            assert "dummy-refresh-token" not in all_logs

        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()


def main():
    if len(sys.argv) != 2:
        print("Usage: python3 scripts/test-packaged-binary.py /path/to/codex-proxy", file=sys.stderr)
        sys.exit(1)
    binary_path = Path(sys.argv[1]).resolve()
    test_binary(binary_path)
    print(f"Packaged binary smoke tests passed for: {binary_path}")


if __name__ == "__main__":
    main()

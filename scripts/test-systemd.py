#!/usr/bin/env python3
"""Opt-in root/systemd smoke test. Uses only disposable units/state and dummy keys."""

import fcntl
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import urllib.parse


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def request(base, path, key=None):
    headers = {"Authorization": "Bearer " + key} if key else {}
    # Do not inherit an operator's proxy settings for loopback tests.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(urllib.request.Request(base + path, headers=headers), timeout=2) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, None


def ready(base):
    for _ in range(100):
        try:
            status, body = request(base, "/health")
            if status == 200:
                assert body["status"] == "ok" and not body["credential_configured"]
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.1)
    raise RuntimeError("service did not become healthy")


def main():
    if os.geteuid() != 0:
        raise SystemExit("run as root on a disposable Linux/systemd test host")
    if len(sys.argv) != 2:
        raise SystemExit("usage: sudo python3 scripts/test-systemd.py /path/to/codex-proxy")
    run("systemctl", "show", "--property=Version")
    source = Path(__file__).resolve().parent.parent / "deploy/systemd/codex-proxy.service"
    binary = Path(sys.argv[1]).resolve(strict=True)
    # /run is visible with PrivateTmp/ProtectHome; never install the production service.
    work = Path(tempfile.mkdtemp(prefix="codex-proxy-smoke-", dir="/run"))
    name = work.name
    unit = name + ".service"
    unit_path = Path("/run/systemd/system") / unit
    state = Path("/var/lib") / name
    private_state = Path("/var/lib/private") / name
    assert not state.exists() and not private_state.exists() and not unit_path.exists()
    try:
        work.chmod(0o755)
        shutil.copyfile(binary, work / "proxy")
        (work / "proxy").chmod(0o755)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        base = "http://127.0.0.1:" + str(port)
        env_file = work / "test.env"
        template = (source.parent / "codex-proxy.env.example").read_text()
        default_env = template.replace("CODEX_PROXY_ADDR=127.0.0.1:8081", f"CODEX_PROXY_ADDR=127.0.0.1:{port}")
        default_env = default_env.replace("CODEX_PROXY_API_KEY=\n", "CODEX_PROXY_API_KEY=systemd-smoke-inference\n")
        env_file.write_text(default_env.replace("CODEX_PROXY_MANAGEMENT_KEY=\n", "CODEX_PROXY_MANAGEMENT_KEY=systemd-smoke-management\n"))
        env_file.chmod(0o600)
        # Run inside the exact same sandbox before the server starts. Test private
        # atomic writes, a loopback callback listener, zero capabilities, and a
        # read-only system filesystem without contacting any OAuth provider.
        probe = work / "probe.py"
        probe.write_text('''import os, pathlib, socket
assert os.getuid() != 0
status = pathlib.Path("/proc/self/status").read_text()
assert "NoNewPrivs:\\t1" in status
caps = next(line.split()[1] for line in status.splitlines() if line.startswith("CapEff:"))
assert int(caps, 16) == 0
assert os.statvfs("/usr").f_flag & os.ST_RDONLY
assert not os.access(os.environ["SMOKE_ENV_FILE"], os.R_OK | os.W_OK)
for name in ("auth", "history"):
    directory = pathlib.Path.cwd() / name
    directory.mkdir(mode=0o700, exist_ok=True)
    assert directory.stat().st_mode & 0o777 == 0o700
    temp = directory / "probe.tmp"
    temp.write_text("persistent-smoke-marker")
    with temp.open("rb") as stream:
        os.fsync(stream.fileno())
    temp.replace(directory / "probe-marker")
    fd = os.open(directory, os.O_RDONLY)
    os.fsync(fd)
    os.close(fd)
with socket.socket() as callback:
    callback.bind(("127.0.0.1", 0))
    callback.listen()
''')
        probe.chmod(0o644)
        text = source.read_text()
        text = text.replace("User=codex-proxy\n", "User=" + name + "\n")
        text = text.replace("StateDirectory=codex-proxy\n", "StateDirectory=" + name + "\n")
        text = text.replace("/var/lib/codex-proxy", str(state))
        text = text.replace("/etc/codex-proxy/codex-proxy.env", str(env_file))
        text = text.replace("/usr/local/bin/codex-proxy", str(work / "proxy"))
        text = text.replace("ExecStart=", f"Environment=SMOKE_ENV_FILE={env_file}\nExecStartPre=/usr/bin/python3 {probe}\nExecStart=", 1)
        unit_path.write_text(text)
        run("systemd-analyze", "verify", str(unit_path))
        run("systemctl", "daemon-reload")
        run("systemctl", "start", unit)
        ready(base)
        assert request(base, "/v1/models")[0] == 401
        assert request(base, "/v1/models", "systemd-smoke-inference")[0] == 503
        assert request(base, "/status/account")[0] == 401
        assert request(base, "/status/account", "systemd-smoke-inference")[0] == 401
        assert request(base, "/status/account", "systemd-smoke-management")[0] == 503
        status_code, login = request(base, "/config/login", "systemd-smoke-management")
        assert status_code == 200
        redirect = urllib.parse.parse_qs(urllib.parse.urlsplit(login["login_url"]).query)["redirect_uri"][0]
        callback = urllib.parse.urlsplit(redirect)
        assert callback.hostname == "127.0.0.1" and callback.port in (1455, 1457)
        with socket.create_connection((callback.hostname, callback.port), timeout=2):
            pass
        # Never open/log the authorization URL or exchange any real credentials.
        pid = int(run("systemctl", "show", "--property=MainPID", "--value", unit))
        status = Path(f"/proc/{pid}/status").read_text()
        uid = next(line.split()[1] for line in status.splitlines() if line.startswith("Uid:"))
        assert int(uid) != 0
        assert state.stat().st_mode & 0o777 == 0o700
        assert (work / "proxy").stat().st_uid == 0
        assert env_file.stat().st_mode & 0o777 == 0o600
        lock = state / "auth/auth.json.codex-proxy.lock"
        history_locks = list((state / "history").glob("*.lock"))
        assert history_locks
        locks = [lock, *history_locks]
        lock_inodes = [path.stat().st_ino for path in locks]
        # The running process really owns both locks; stopping must release them.
        for path in locks:
            with path.open("rb") as stream:
                try:
                    fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    pass
                else:
                    raise AssertionError("state lock was not held")
        run("systemctl", "stop", unit)
        assert run("systemctl", "show", "--property=ExecMainStatus", "--value", unit) == "0"
        for path in locks:
            with path.open("rb") as stream:
                fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        for directory in ("auth", "history"):
            assert (state / directory / "probe-marker").read_text() == "persistent-smoke-marker"
        run("systemctl", "start", unit)
        ready(base)
        assert [path.stat().st_ino for path in locks] == lock_inodes
        run("systemctl", "stop", unit)
        # Exercise the shipped management-disabled default as a separate start.
        env_file.write_text(default_env)
        run("systemctl", "start", unit)
        ready(base)
        assert request(base, "/status/account", "systemd-smoke-management")[0] == 503
        run("systemctl", "stop", unit)
        print("systemd smoke passed: non-root sandbox, auth, atomic state writes, locks, restart")
    except Exception:
        subprocess.run(["journalctl", "--no-pager", "-u", unit, "-n", "40"], check=False)
        raise
    finally:
        stopped = subprocess.run(["systemctl", "stop", unit], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if stopped.returncode != 0 and unit_path.exists():
            raise RuntimeError(f"cleanup could not stop {unit}; retaining test files in {work}")
        unit_path.unlink(missing_ok=True)
        subprocess.run(["systemctl", "daemon-reload"], check=False)
        subprocess.run(["systemctl", "reset-failed", unit], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        # Never remove live lock/state files until the test service has stopped.
        if state.is_symlink():
            state.unlink()
        elif state.exists():
            shutil.rmtree(state)
        if private_state.exists():
            shutil.rmtree(private_state)
        shutil.rmtree(work)


if __name__ == "__main__":
    main()

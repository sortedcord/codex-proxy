#!/usr/bin/env python3
"""Validate deploy/compose.yaml structure, interpolation, and security configuration."""

import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest


def interpolate_compose(text: str, env: dict[str, str]) -> str:
    """Simulate Compose variable interpolation: ${VAR:-default} and ${VAR:?error}."""
    def replace(match):
        var_name = match.group(1)
        sep = match.group(2)
        tail = match.group(3)
        val = env.get(var_name, "")
        if sep == ":?":
            if not val:
                raise ValueError(tail or f"{var_name} is required")
            return val
        if sep == ":-":
            return val if val else tail
        return val

    pattern = re.compile(r"\$\{([A-Za-z0-9_]+)(:[?-])([^}]*)\}")
    return pattern.sub(replace, text)


class TestComposeConfig(unittest.TestCase):
    def setUp(self):
        self.compose_path = Path(__file__).resolve().parent.parent / "deploy/compose.yaml"
        self.raw_yaml = self.compose_path.read_text()

    def test_missing_api_key_fails_interpolation(self):
        with self.assertRaises(ValueError) as ctx:
            interpolate_compose(self.raw_yaml, {})
        self.assertIn("CODEX_PROXY_API_KEY is required", str(ctx.exception))

    def test_empty_api_key_fails_interpolation(self):
        with self.assertRaises(ValueError) as ctx:
            interpolate_compose(self.raw_yaml, {"CODEX_PROXY_API_KEY": ""})
        self.assertIn("CODEX_PROXY_API_KEY is required", str(ctx.exception))

    def test_valid_interpolation_defaults(self):
        env = {"CODEX_PROXY_API_KEY": "test-key-123"}
        interpolated = interpolate_compose(self.raw_yaml, env)
        self.assertIn("ghcr.io/sortedcord/codex-proxy:0.5.0", interpolated)
        self.assertIn("127.0.0.1:8081:8080", interpolated)
        self.assertIn("CODEX_PROXY_API_KEY: test-key-123", interpolated)
        self.assertIn("read_only: true", interpolated)
        self.assertIn("no-new-privileges:true", interpolated)
        self.assertIn("- ALL", interpolated)
        self.assertIn("source: codex-proxy-auth", interpolated)
        self.assertIn("target: /home/app/.codex", interpolated)
        self.assertIn("source: codex-proxy-history", interpolated)
        self.assertIn("target: /home/app/.local/share/codex-proxy", interpolated)

    def test_overrides(self):
        env = {
            "CODEX_PROXY_API_KEY": "test-key-123",
            "CODEX_PROXY_IMAGE": "custom-image:tag",
            "CODEX_PROXY_BIND_ADDR": "127.0.0.1:9090",
            "CODEX_PROXY_MANAGEMENT_KEY": "mgmt-key-456",
        }
        interpolated = interpolate_compose(self.raw_yaml, env)
        self.assertIn("custom-image:tag", interpolated)
        self.assertIn("127.0.0.1:9090:8080", interpolated)
        self.assertIn("CODEX_PROXY_MANAGEMENT_KEY: mgmt-key-456", interpolated)

    def test_docker_compose_cli_if_available(self):
        if not shutil.which("docker"):
            return
        with tempfile.NamedTemporaryFile("w", suffix=".env") as temp_env:
            temp_env.write("CODEX_PROXY_API_KEY=dummy-inference-key\n")
            temp_env.flush()
            result = subprocess.run(
                ["docker", "compose", "--env-file", temp_env.name, "-f", str(self.compose_path), "config"],
                capture_output=True,
                text=True,
            )
            if result.returncode != 0:
                self.fail(f"docker compose config failed:\n{result.stderr}")


if __name__ == "__main__":
    unittest.main()

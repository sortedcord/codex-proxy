# Changelog

All notable changes to this project will be documented in this file.
This project adheres to [Semantic Versioning](https://semver.org/).

## 0.5.0 (2026-10-13)

### Features

- Formalize v0.5.0 baseline versioning, build identity, and `codex-proxy version` command.
- Add automated GoReleaser release pipeline for Linux, macOS, and Windows standalone binaries.
- Add hardened Docker Compose deployment configuration (`deploy/compose.yaml`).
- Add hardened Linux systemd service unit (`deploy/systemd/codex-proxy.service`).
- Separate stable and development container release channels on GHCR.
- Add release artifact and deployment example smoke tests in CI.

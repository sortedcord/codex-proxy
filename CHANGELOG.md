# Changelog

All notable changes to this project will be documented in this file.
This project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Features

* **accounts:** add multi-account support with account-scoped API keys (`cxp_...`)
* **admin:** add Admin API protected by `ADMIN_API_KEY` for accounts, keys, login sessions, and quota
* **cli:** add CLI commands for account and API key management (`accounts`, `keys`, `admin-key`, `login`)
* **config:** automatically migrate single-account credentials to multi-account format without modifying legacy files
* **quota:** add multi-account quota aggregation and per-account status history

## [0.6.0](https://github.com/sortedcord/codex-proxy/compare/v0.5.0...v0.6.0) (2026-10-13)


### ⚠ BREAKING CHANGES

* use /health instead of /healthz and build ./cmd/codex-proxy instead of the repository root.

### Features

* add lightweight Codex Responses proxy ([061905c](https://github.com/sortedcord/codex-proxy/commit/061905c6fae343faf3e373c354dd46a7b15485e1))
* add secure runtime account management ([18ca267](https://github.com/sortedcord/codex-proxy/commit/18ca2672d39b48f9ecc3b0284fba0db22fcb0baa))
* **ci:** separate stable and development container channels ([#5](https://github.com/sortedcord/codex-proxy/issues/5)) ([680097d](https://github.com/sortedcord/codex-proxy/commit/680097da5df2591c8a95eaa4b6572eb575e9c8bf))
* **ci:** separate stable and development container release channels ([6c606e7](https://github.com/sortedcord/codex-proxy/commit/6c606e759f3221fdbce6e07516dea10e1ed1be95))
* **deploy:** add hardened systemd service ([#7](https://github.com/sortedcord/codex-proxy/issues/7)) ([66967b1](https://github.com/sortedcord/codex-proxy/commit/66967b180fa1d48575253b83bd3cc2ff6003a313))
* **deploy:** ship hardened Docker Compose deployment example ([3330573](https://github.com/sortedcord/codex-proxy/commit/3330573aba29891d27c8c1b997e6ef5158507829))
* **deploy:** ship hardened Docker Compose deployment example ([#6](https://github.com/sortedcord/codex-proxy/issues/6)) ([3d5c737](https://github.com/sortedcord/codex-proxy/commit/3d5c7376f23aa457df8051729df66d069c12b36e))
* formalize v0.5.0 versioning and identify builds ([59e78db](https://github.com/sortedcord/codex-proxy/commit/59e78db8899c5b4eda41fffd1fcb151caf1d3a09))
* provide a hardened systemd service for Linux deployments ([eca28dd](https://github.com/sortedcord/codex-proxy/commit/eca28dd20cbbc1ad46d40ef3efc32c90f4bbb708))
* **quota:** standardize limit windows to five_hour and weekly ([46b624e](https://github.com/sortedcord/codex-proxy/commit/46b624e1a44f0f0a68dd574a8cdab3645f57ae07))
* **release:** publish versioned native binaries via automated pipeline ([#3](https://github.com/sortedcord/codex-proxy/issues/3)) ([bbab9e0](https://github.com/sortedcord/codex-proxy/commit/bbab9e0532080ef99b97fe2b9c4076b2a6cff9db))
* **release:** publish versioned native binaries via automated release pipeline ([31ad07f](https://github.com/sortedcord/codex-proxy/commit/31ad07fd51c48be4e5622bc881697096a8112cbc))
* **version:** identify v0.5.0 builds ([#4](https://github.com/sortedcord/codex-proxy/issues/4)) ([e7cc5a7](https://github.com/sortedcord/codex-proxy/commit/e7cc5a70d8d1a73ae2bcb8531f6b6f4b38f77b9b))


### Bug Fixes

* **docker:** include internal/httputil in docker build context ([ed13d6c](https://github.com/sortedcord/codex-proxy/commit/ed13d6ce345db922e927327715905c7d9827e193))
* streaming lifecycle, credential caching, and reliability ([d3a8505](https://github.com/sortedcord/codex-proxy/commit/d3a8505dfbe591c877cb9f15c8897de7992075ac))
* support Bifrost Responses provider ([33ff298](https://github.com/sortedcord/codex-proxy/commit/33ff2986db1c6a48592adb62dce38166f9562d12))
* **test:** adapt POSIX permission checks and process termination for Windows ([#8](https://github.com/sortedcord/codex-proxy/issues/8)) ([4cf3929](https://github.com/sortedcord/codex-proxy/commit/4cf3929d78e12820be3b66cc84d545674e4d703a))

## 0.5.0 (2026-10-13)

### Features

- Formalize v0.5.0 baseline versioning, build identity, and `codex-proxy version` command.
- Add automated GoReleaser release pipeline for Linux, macOS, and Windows standalone binaries.
- Add hardened Docker Compose deployment configuration (`deploy/compose.yaml`).
- Add hardened Linux systemd service unit (`deploy/systemd/codex-proxy.service`).
- Separate stable and development container release channels on GHCR.
- Add release artifact and deployment example smoke tests in CI.

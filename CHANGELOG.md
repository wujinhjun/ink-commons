# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Protocol-neutral Adapter HTTP v1 & multi-provider routing**:
  - Formalized [Adapter HTTP v1](docs/adapter-http-v1.md) covering `placement`, `prepare`, `deliver`, `wake`, `notify`, `blocked_notice`, `conversation_context`, `status`, and `stop`, plus startup authenticated GET admission for `managed: true` providers.
  - Added top-level named `providers` configuration map so a single daemon can route across multiple independent HTTP adapters.
  - Extracted the `cc-connect` / Discord integration into a standalone binary (`cmd/ink-adapter-ccconnect`), removing all `ccconnect` dependencies from `cmd/ink` and `internal/ink`.
  - Added a standard-library minimal HTTP executor example and end-to-end test in `examples/http-executor/`.
- **Role-isolated GitHub credentials, merge strategy boundary, and conditional polling**:
  - Introduced role-isolated, cached `TokenProvider` (`github_reader` and `github_merger`) supporting PAT (`token_env`), native GitHub App installations (`app`), and external credential helpers (`helper`), while forbidding implicit fallback to personal `gh` credentials.
  - Extracted `MergeStrategy` and `Merger` interfaces for the v0.1 SHA-locked `direct` and Stack PR merge engine.
  - Added scoped in-memory `ETag` / `If-None-Match` conditional GET caching for paginated PR and review observations, with fresh uncached preflight reads and primary/secondary rate-limit handling.
- **Storage, concurrency, and observability**:
  - Upgraded to SQLite schema v7 with `//go:embed` single-source `sqlc` DDL, `mode=rw` anti-empty-database guardrails (`ink state init`), atomic v6→v7 migration, and per-PR transactional persistence with optimistic revision checks and pending partial indexes.
  - Replaced serial polling with bounded concurrency (4 repository discovery workers, 8 PR sync workers, strict per-PR mutex serialization, and a dedicated 2-worker `post_approval` hook pool).
  - Upgraded daemon logging to structured `log/slog` with configurable `log_level` and `log_format` (`text` / `json`) and diagnostic redaction.
- **Unified configuration, Schema, and operations CLI**:
  - Unified JSON configuration naming to `snake_case` (`post_approval`), added complete `config.example.json` and JSON Schema (`config.schema.json`), and unified static/dynamic policy validation.
  - Migrated CLI parsing to `cobra`/`pflag` with persistent `--data` and `--json` flags, atomic `PATCH /v1/repositories`, safe `DELETE /v1/repositories` lifecycle guards, and full operational commands (`ink work list|show|link|assign|retry|confirm-stopped`, `ink repo list|add|update|remove`, `ink doctor`).
- **Pure-Go cross-platform build, portable service templates, and open-source baseline**:
  - Added `Makefile`, `.golangci.yml`, `.goreleaser.yaml`, Linux + macOS GitHub Actions CI (`ci.yml`) and tag release workflow (`release.yml`), and portable `systemd` / `launchd` templates under `deploy/`.
  - Added bilingual homepages (`README.md` + `README.zh-CN.md`), `docs/architecture.md`, `docs/deployment.md`, `docs/adapters.md`, `docs/cli.md`, MIT `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, and GitHub Issue/PR templates.

### Changed

- Updated Go module path to `github.com/wujinhjun/ink-commons` and added explicit POSIX build constraints (`//go:build linux || darwin`) across `cmd/` and `internal/ink/`.
- Unified `postApproval` configuration and API keys to `post_approval` (with automatic locked v6→v7 SQLite upgrade).

### Removed

- Removed legacy `state.json` migration paths, legacy compatibility route fields, and `migrateExecution`.
- Removed private macOS Python deployment scripts (`deploy/macos/`, `cmd/ink/upgrade.go`, `tests/macos/`), making the repository 100% pure Go.

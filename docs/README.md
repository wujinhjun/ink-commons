# Ink Commons Documentation

Welcome to the Ink Commons documentation index. For a 5-minute quickstart, see [`README.md`](../README.md) ([简体中文](../README.zh-CN.md)).

## Core Architecture & Operations

- [Architecture & Dual-Layer State Machine](architecture.md): System components, `phase` × `workflow.state` × `attempt.state` / `handoff.state` diagrams, bounded concurrency, SQLite v7 persistence, and trust boundaries.
- [CLI Reference](cli.md): Full command reference for `work`, `repo`, `config`, `task`, `notification`, `context`, `workspace`, `state`, `doctor`, `init`, and `hook` (with `--data` and `--json`).
- [Deployment & Hardening Guide](deployment.md): Linux `systemd` and macOS `launchd` setup, service account isolation, local Git workspace pool requirements, skills/hooks, and backups.
- [Service Templates](../deploy/README.md): Portable `systemd` units and `launchd` plists under `deploy/`.
- [Single-Instance Cutover & Migration Checklist (中文)](migration.md): Upgrading SQLite v6→v7, clean cutovers from older schemas, and migrating between hosts.

## Protocol, Credentials & Adapters

- [Configure and Run HTTP Adapters](adapters.md): Multi-provider `providers` map, running `ink-adapter-ccconnect`, and implementing custom adapters.
- [Adapter HTTP v1 Wire Contract](adapter-http-v1.md): Request/response schemas, capability declarations, placement/allocation recovery, conversation context, and `managed: true` admission.
- [Minimal HTTP Executor Example](../examples/http-executor/README.md): Standard-library reference adapter (`prepare` + `deliver`) and zero-credential end-to-end test.
- [GitHub Credential & Merge Contracts](github-contracts.md): Role-isolated `TokenProvider` (`github_reader` / `github_merger`), `MergeStrategy` / `Merger`, and scoped paginated `ETag` caching.

## Policy, Execution & Handoff Deep Dives

- [Centralized Configuration (中文)](configuration.md): Static vs SQLite runtime scopes, precedence, validation, `snake_case` keys (`post_approval`), `config.schema.json`, and safe repository removal.
- [Execution Lifecycle & Durable Executor Integration](execution.md): `task_id` / `attempt_id`, Monitor `status`/`stop` receipts, internal SQLite executor ledger, and operator recovery (`confirm-stopped`).
- [Cooperative Claim & Business Handoff (中文)](handoff.md): `task_mode: "cooperative"`, durable claim credentials, idempotent submission, GitHub evidence verification, and blocked notices.

## Project Policies

- [Security Policy](../SECURITY.md) · [Contributing](../CONTRIBUTING.md) · [Changelog](../CHANGELOG.md) · [Roadmap](../TODO.md) · [MIT License](../LICENSE)

## Workflow Diagram

The [cooperative workflow diagram](diagrams/cooperative-workflow.png) illustrates first-time development, review assignment, rework, cooperative handoff verification, and optional auto-merge alongside placeholder configuration callouts:

- Rendered PNG: [`diagrams/cooperative-workflow.png`](diagrams/cooperative-workflow.png)
- Editable Excalidraw source: [`diagrams/cooperative-workflow.excalidraw`](diagrams/cooperative-workflow.excalidraw)

> Note: All configuration values in the diagram are placeholders; see [`config.example.json`](../config.example.json) and [Centralized Configuration](configuration.md) for the complete schema and policy rules.

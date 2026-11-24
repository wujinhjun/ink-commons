# Security Policy

## Supported Versions

Security fixes are applied to the latest release series (`v0.1.x`) and the `main` branch. Ink Commons supports **Linux** and **macOS** only (POSIX build constraints exclude Windows).

## Security Architecture and Trust Boundaries

Ink Commons is designed as a single-host, least-privilege GitHub PR orchestrator with explicit trust boundaries:

- **Private Local Control Plane (`0600` Unix Socket + UID)**: All operator commands, agent cooperative handoffs (`claim`, `submit`, `retry`), notification ACKs, and review context reads go through `ink.sock` inside the daemon's state directory. The state directory must be mode `0700`; the socket, SQLite database (`state.sqlite3`), lock file, and claim credential files (`claims/<task_id>.json`) enforce mode `0600` and ownership by the dedicated unprivileged service UID. Never expose `ink.sock` or local daemon APIs to a network interface.
- **Unprivileged Execution**: Run `ink` and any external adapter under dedicated unprivileged service accounts. Workspace pool operations (`ink workspace`) explicitly refuse to run when `euid == 0`.
- **No Shell Expansion**: External commands (`post_approval` hooks, GitHub credential helpers, legacy CLI wrappers, and `git` subprocesses) are executed directly via `argv` arrays with absolute executable paths—never through a shell—and never execute commands declared inside pull requests.
- **Strict Input Parsing & HTTP Hardening**:
  - Local API, static configuration, and Adapter HTTP v1 responses use strict JSON decoding (`DisallowUnknownFields`).
  - Remote Adapter HTTP endpoints require HTTPS and a nonempty Bearer token (`token_env`); HTTP redirects are refused, and requests/responses are capped at 1 MiB.
- **GitHub Credential Isolation & No Implicit Fallback**:
  - `github_reader` and `github_merger` use separate role scopes and cache entries.
  - Implicit fallback to a personal `gh` login, ambient Git/GitHub CLI tokens, or the other role's credential is forbidden. Subprocesses run with stripped ambient Git/GitHub credential environment variables.
  - GitHub App private keys must be mode `0600` or stricter. Legacy `github_merger_command` requires SHA-256 binary pinning (`github_merger_command_sha256`).
  - Shared reader/merger App identity requires explicit per-repository opt-in (`auto_merge.reuse_reader_identity: true`).
- **Adapter Trust Boundary & `managed: true` Admission**:
  - External adapters do not receive access to `ink.sock`, the SQLite database, or cooperative claim credentials.
  - Enabling `managed: true` requires a Bearer token, `deliver` / `status` / `stop` capabilities, and a startup authenticated GET admission declaration asserting durable delayed-delivery fencing and real runtime/tool termination.
- **Structured Log Redaction**: Structured logs (`log/slog`) record operational metadata (operation, repo, PR number, alert code, HTTP/SQLite status codes) while avoiding token material, conversation bodies, raw configuration snapshots, and sensitive local paths. Treat SQLite backups and JSON exports as sensitive operational data.

## Reporting a Vulnerability

Please **do not** report security vulnerabilities through public GitHub issues.

Instead, report vulnerabilities privately using [GitHub Security Advisories](https://github.com/wujinhjun/ink-commons/security/advisories/new) for this repository. Include:

1. A description of the vulnerability and affected component(s) (`cmd/ink`, `internal/ink`, `cmd/ink-adapter-ccconnect`, etc.).
2. Reproduction steps or a proof of concept.
3. Any suggested mitigation or patch, if available.

We aim to acknowledge reports within 72 hours and coordinate a fix and disclosure timeline with the reporter.

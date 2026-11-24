# Contributing to Ink Commons

Thank you for your interest in contributing to Ink Commons!

## Development Requirements

- **Operating System**: Linux or macOS (POSIX build constraints `//go:build linux || darwin`; Windows is not supported).
- **Go**: Go 1.23 or newer (CI currently runs Go 1.26.x).
- **Git**: Required for local workspace and hook tests.
- **Optional local tools**: `golangci-lint` v2 and `sqlc` v1.30.0 (if `sqlc` is not installed locally, `scripts/generate-sqlc.sh` falls back to `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0`).

## Building and Verifying Changes

Run the full local verification suite before opening a pull request:

```sh
make build
make test-race vet lint sqlc-check
```

- `make build`: Builds `bin/ink` and `bin/ink-adapter-ccconnect` with `CGO_ENABLED=0`.
- `make test-race`: Runs all unit and integration tests with the race detector (`go test -race -count=1 ./...`).
- `make vet`: Runs `go vet ./...`.
- `make lint`: Runs `golangci-lint run` using `.golangci.yml`.
- `make sqlc-check`: Regenerates `sqlc` code and verifies `internal/ink/statedb` and `internal/execution/ledger/ledgerdb` have no uncommitted drift.

### SQLite Schema and `sqlc` Changes

`internal/ink/sql/schema.sql` and `internal/execution/ledger/sql/schema.sql` are the single source of truth for both runtime DDL (`//go:embed`) and `sqlc` code generation. After modifying any SQL schema or query:

1. Run `./scripts/generate-sqlc.sh`.
2. Commit the regenerated files under `internal/ink/statedb/` or `internal/execution/ledger/ledgerdb/`.
3. Verify with `make sqlc-check`.

## Architectural Guardrails

- **Core remains protocol-neutral**: `cmd/ink` and `internal/ink` must never import `internal/execution/ccconnect` or runtime-specific chat SDKs (enforced by CI). Keep runtime-specific adapters in separate binaries or external repositories implementing [Adapter HTTP v1](docs/adapter-http-v1.md).
- **GitHub PR snapshot is the business source of truth**: Execution acceptance or completion alone must never advance business `phase`.
- **Persist before external side effects**: Reserve deliveries, approval hooks, notifications, session allocations, and merge intents in SQLite before making external calls.
- **No personal data or secrets**: Use placeholder identities (`owner/repo`, `reviewer-bot`, `example/project`) in tests, docs, and examples.

## Pull Request Guidelines

1. Keep pull requests focused on one clear change.
2. Add or update unit/integration tests covering new behavior and edge cases.
3. Keep documentation (`README.md`, `README.zh-CN.md`, `docs/`, `config.schema.json`, `config.example.json`) synchronized with code changes—zero drift.
4. Fill out the PR template checklist and confirm `make build test-race vet lint sqlc-check` passes cleanly.

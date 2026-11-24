# Deployment and Operations Guide

Ink Commons supports **Linux** and **macOS** (`amd64` and `arm64`). Windows is not supported (POSIX build constraints enforce `//go:build linux || darwin`).

For upgrading an existing instance or moving state between hosts, also review the [single-instance cutover checklist](migration.md).

## 1. Install Binaries

### Option A: Release Archives

Download the `tar.gz` archive and `SHA256SUMS` for your platform from GitHub Releases, verify the checksum, and install `ink` (and optionally `ink-adapter-ccconnect`) to an administrator-controlled directory such as `/usr/local/bin`:

```sh
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf ink-commons_<version>_<os>_<arch>.tar.gz
sudo install -m 0755 ink /usr/local/bin/ink
# Optional: if using the standalone cc-connect adapter
sudo install -m 0755 ink-adapter-ccconnect /usr/local/bin/ink-adapter-ccconnect
```

### Option B: Build from Source

Requires Go 1.23+ and Git:

```sh
make build
# Produces CGO_ENABLED=0 binaries: bin/ink and bin/ink-adapter-ccconnect
```

Verify the binary revision:

```sh
ink version --json
```

## 2. Service Accounts, Directories, and Security Hardening

Run the daemon and any external adapter under **dedicated unprivileged service accounts**:

- **State directory (`0700`)**: Owned by the daemon service user. Keep the path short so `<state_dir>/ink.sock` stays within the OS Unix domain socket path limit (~104 bytes on macOS, ~108 bytes on Linux).
- **Private Unix socket (`0600`)**: `ink serve` creates `ink.sock` with mode `0600` owned by the daemon UID. Run local `ink` CLI commands (and local agent handoffs) as that service user with the same `--data` or `INK_DATA_DIR`. Never expose `ink.sock` over a network proxy.
- **SQLite database (`0600`)**: Initialize a fresh state directory explicitly before the first start:

  ```sh
  sudo -u ink ink state init --data /var/lib/ink-commons
  ```

- **Secrets and environment files (`0600`)**: Keep daemon secrets (`github_reader` / `github_merger` PATs, GitHub App `0600` PEM keys, provider Bearer tokens) and adapter secrets (`INK_ADAPTER_TOKEN`, `INK_CC_CONNECT_TOKEN`, `INK_DISCORD_TOKEN`) in separate environment files readable only by the respective service account (or root-owned `0600` for systemd `EnvironmentFile`).
- **Adapter isolation**: Bind `ink-adapter-ccconnect` to loopback (`127.0.0.1:8788`). Do not grant the adapter access to the daemon state directory, `ink.sock`, or cooperative claim credentials.
- **No implicit `gh` fallback**: Configure `github_reader` (and `github_merger` if auto-merge is enabled) explicitly via `token_env`, `app`, or `helper` in `config.json`. See [GitHub credential contracts](github-contracts.md) and [`config.example.json`](../config.example.json).

## 3. Linux (`systemd`)

Portable unit templates live in [`deploy/systemd/`](../deploy/systemd/) (see also [`deploy/README.md`](../deploy/README.md)).

1. Copy [`deploy/systemd/ink.service`](../deploy/systemd/ink.service) (and optionally [`deploy/systemd/ink-adapter-ccconnect.service`](../deploy/systemd/ink-adapter-ccconnect.service)) to `/etc/systemd/system/`.
2. Replace every placeholder (`INK_SERVICE_USER`, `INK_SERVICE_GROUP`, `/path/to/...`, `INK_PROVIDER_NAME`).
3. Create root-owned `0600` environment files (e.g. `/etc/ink-commons/daemon.env` and `/etc/ink-commons/adapter.env`).
4. Initialize the state directory as the daemon user:

   ```sh
   sudo install -d -o ink -g ink -m 0700 /var/lib/ink-commons
   sudo -u ink /usr/local/bin/ink state init --data /var/lib/ink-commons
   ```

5. Validate and start the services:

   ```sh
   sudo systemd-analyze verify /etc/systemd/system/ink.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now ink.service
   ```

The `systemd` unit enforces `StateDirectoryMode=0700`, `UMask=0077`, `NoNewPrivileges=yes`, `PrivateTmp=yes`, and `ProtectSystem=full`. The adapter template additionally sets `ProtectSystem=strict` and `ProtectHome=yes`.

## 4. macOS (`launchd`)

Portable LaunchDaemon templates live in [`deploy/launchd/`](../deploy/launchd/) (see also [`deploy/README.md`](../deploy/README.md)).

1. Copy [`deploy/launchd/org.example.ink.plist`](../deploy/launchd/org.example.ink.plist) (and optionally [`deploy/launchd/org.example.ink-adapter-ccconnect.plist`](../deploy/launchd/org.example.ink-adapter-ccconnect.plist)) to `/Library/LaunchDaemons/` with your service label, owned by `root:wheel` and mode `0644`.
2. Replace every placeholder (`INK_SERVICE_USER`, `INK_SERVICE_GROUP`, `/path/to/...`, `INK_PROVIDER_NAME`).
3. Create the state directory owned by the daemon service user with mode `0700`, and initialize the database as that user:

   ```sh
   sudo install -d -o ink -g staff -m 0700 /var/db/ink-commons
   sudo -u ink /usr/local/bin/ink state init --data /var/db/ink-commons
   ```

4. Create the service environment file(s) with trusted POSIX `KEY=VALUE` assignments readable only by the respective service account (`0600`).
5. Validate and bootstrap:

   ```sh
   plutil -lint /Library/LaunchDaemons/org.example.ink.plist
   sudo launchctl bootstrap system /Library/LaunchDaemons/org.example.ink.plist
   ```

The launchd templates enforce `Umask` `63` (`0077` octal) and source the environment file as the service user before `exec`ing the binary.

## 5. Workspace Pool Requirements (Full / Lite Worktrees)

If you use `ink workspace` (`list`, `acquire`, `release`, `remove`) to manage Full/Lite Git worktree pools (`workspace_full_slots`):

- **Local mounted/synchronized Git clone required**: `ink workspace` operates directly on **local Git clones or worktrees on the same host**. It does not clone remote repositories automatically—prepare and fetch the required refs in the local clone first, then pass `--repo /path/to/local/clone`.
- **Unprivileged daemon required**: Workspace APIs explicitly reject execution when `euid == 0` (`403 workspace operations require an unprivileged daemon`) because Git reads repository configuration and filters.
- **Filesystem permissions & systemd hardening**: The daemon service account must have read/write permission to the local Git clone and worktree parent directories, and `git` must be available in the service's `PATH`. Under `systemd`, `ProtectSystem=full` allows writable paths outside `/usr`, `/boot`, and `/etc`; if your local clones live under `/home` or another restricted path, ensure `ProtectHome` is not blocking access and add explicit `ReadWritePaths=` if you tighten sandboxing further.
- **Clean release invariants**: `ink workspace release` requires a clean working tree (no uncommitted or untracked files) and verifies that `HEAD` is reachable on `origin`. Full slots retain directory structure and ignored caches for reuse; Lite slots can be explicitly removed with `ink workspace remove`.

## 6. Agent Workflow Skills & Git Pre-Push Hook

### Agent Workflow Skills

Install the embedded `ink-commons-workflow` skill as the user running the agent:

```sh
ink init
ink init --check
```

`ink init` installs the versioned skill bundle into `${XDG_DATA_HOME:-$HOME/.local/share}/ink-commons/skills/<hash>/` and symlinks `~/.agents/skills/ink-commons-workflow`. Pass `--skills-dir /path/to/skills` consistently to both commands when using a custom discovery directory. Re-run `ink init` and `ink init --check` after upgrading `ink`, then start a new agent session.

### Optional Git Pre-Push Provenance Hook

To associate development pushes with PRs automatically (instead of running `ink work link` manually), install the pre-push hook inside a developer checkout:

```sh
ink hook install --checkout /path/to/checkout
ink hook status --checkout /path/to/checkout
```

The installed hook resolves the `ink` binary in this order:

1. `INK_BIN` (if set in the Git push environment—use this for source builds such as `INK_BIN=/absolute/path/to/bin/ink git push`, or add `bin/` to `PATH`).
2. `ink` on `PATH` (for example `/usr/local/bin/ink` from a release installation).
3. `$HOME/.local/lib/ink-commons/ink` (fallback for user-local installations).

You can also install [`scripts/ink-pre-push`](../scripts/ink-pre-push) directly as an executable `pre-push` hook at `$(git rev-parse --git-path hooks/pre-push)`. When wrapping an existing `pre-push` hook, buffer stdin so both hooks receive the ref updates and preserve the existing hook's exit code:

```sh
updates=$(mktemp) || exit 1
trap 'rm -f "$updates"' EXIT
cat > "$updates" || exit 1
/absolute/path/to/existing-pre-push "$@" < "$updates" || exit "$?"
"${INK_BIN:-ink}" pre-push "$2" < "$updates" || :
```

## 7. Verification, Staged Cutover, and Backups

### Health Checks

Run `ink doctor` as the daemon service user to verify state permissions, static configuration, `/healthz`, and `/v1/self-check` in one command:

```sh
ink doctor --config /path/to/ink/config.json --data /var/lib/ink-commons --json
```

### Staged Rollout (`--config-only` and `--poll-gate`)

- `--config-only`: Starts the local Unix socket API for inspecting/updating runtime configuration, reassigning work items, recording `confirm-stopped`, and submitting cooperative handoffs while keeping GitHub polling and new task claims paused.
- `--poll-gate /path/to/gate-file`: Starts the daemon with polling and mutating APIs paused until the specified gate file exists (useful for verifying `/healthz` and `/v1/self-check` before opening traffic).

### Offline Backup & Diagnostics Export

Stop `ink serve` before running snapshots; output paths must not already exist:

```sh
ink state backup --data /var/lib/ink-commons --output /safe/path/ink-backup.sqlite3
ink state export --data /var/lib/ink-commons --output /safe/path/ink-diagnostics.json
```

Both files are written with mode `0600`. Treat SQLite backups and JSON exports as sensitive operational data. To restore, stop the daemon, move the existing `state.sqlite3` aside, verify schema compatibility (schema v7), copy the backup to `state.sqlite3` with mode `0600`, and restart. See [single-instance cutover checklist](migration.md) for cross-version upgrades and host migrations.

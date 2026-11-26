# Ink CLI reference

Run `ink --help` or append `--help` to any command for its flags. Use double-dash flags. Flags can appear before or after positional arguments. Quote work references such as `'owner/repo#42'` in shell commands.

`--data DIR` is a persistent flag. It selects the state directory and Unix socket for local operations. Without it, Ink uses `INK_DATA_DIR`, then `${XDG_STATE_HOME:-$HOME/.local/state}/ink-commons`. Local daemon commands run as the UID that owns the private socket.

Use `--json` for automation. Success writes one JSON document to stdout. Errors exit nonzero and write an object with an `error` string to stderr. Tooling should use the exit code and named JSON fields. Work, repository, task, notification, workspace, and configuration commands return the corresponding local API object or array. Task, notification, context, and config export also use JSON by default. API results can contain private operational data, including routes, worktree paths and conversation history.

## Work items

Work commands accept a GitHub PR URL or an `OWNER/REPO#NUMBER` reference. The daemon must have observed the PR before a work item can be linked or assigned.

```sh
ink work list --json
ink work show 'owner/repo#42'
ink work link 'owner/repo#42' --github-login developer
ink work assign 'owner/repo#42' --role reviewer --github-login reviewer-bot \
  --provider reviews --target review-project --session review-session
ink work retry 'owner/repo#42'
ink work confirm-stopped 'owner/repo#42' --task TASK_ID --attempt ATTEMPT_ID \
  --stopped --reason 'Verified the executor and delayed delivery have stopped'
```

- `list` and `show` read the stored GitHub snapshot and workflow state. JSON includes the task and attempt details needed for follow-up.
- `link` records the developer's origin. Supply a GitHub login or a complete provider, target and session. Use the same route flags with `assign` to change a role's destination. An allocating reviewer route can use `--parent`.
- `retry` resets a retryable legacy task after earlier executions are settled. Cooperative tasks use `ink task retry` with the original claim credential.
- `confirm-stopped` records a verified attempt outcome. Confirm the executor has stopped and cannot restart delayed delivery before recording it.

`ink review --github-login LOGIN PR_URL` remains a shortcut for reviewer assignment. It accepts `--github-command` and the route flags.

## Repositories and configuration

```sh
ink repo list --json
ink repo add owner/repo --github-login reviewer-bot
ink repo update owner/repo --values '{"reviewer_login":"new-reviewer","post_approval":null}'
ink config get reviewer_login --repo owner/repo --effective --json
ink config unset post_approval --repo owner/repo
ink repo remove owner/repo --json
```

- `repo add` and `register` share flags and registration semantics. With no repository argument, they read the current checkout's remote. Pass `--remote NAME` to select another remote.
- `repo update --values JSON` applies an atomic patch of explicit override values. Use dotted names for execution settings, such as `execution.max_attempts`. Use `config unset` to restore inheritance for a key.
- `repo remove` deletes registration and queued push provenance, retires unstarted tasks, and keeps work history and receipts readable. A conflict provides the reason to settle before retrying. See [repository removal](configuration.md#仓库注销) for the safety conditions.
- `config get|set|unset|list` require either `--global` or `--repo OWNER/REPO`. `get` and `list` support `--effective` and, for PR assignments, `--pr NUMBER`. `config export` returns explicit global and repository policy from a single SQLite snapshot.

## Health, state, and setup

| Command | Operation |
| --- | --- |
| `ink doctor [--config FILE]` | Reports `state`, `config`, `healthz` and `self_check` under `checks`. The JSON report includes `ok`. A failed check exits nonzero after emitting the report. Config validation uses the local environment and defaults to `config.json` when present. |
| `ink serve [--config FILE] [--config-only] [--poll-gate FILE]` | Runs the daemon. Config-only mode permits selected local operations while the polling gate is closed. With `--poll-gate`, polling waits until the specified file exists. With `--json`, a clean shutdown emits a stopped receipt. Daemon logs go to stderr. |
| `ink state init` | Creates a state database in a fresh directory. |
| `ink state backup --output FILE` | Writes a SQLite snapshot while the daemon is stopped. |
| `ink state export --output FILE` | Writes a JSON ledger snapshot while the daemon is stopped. |
| `ink init [--check] [--skills-dir DIR]` | Installs or verifies the workflow skill for the current user. `skills` is an alias. An explicit `--data DIR` stores the skill bundle under `DIR/skills`. |
| `ink hook install [--checkout DIR]` | Installs the optional pre-push provenance hook. |
| `ink hook status [--checkout DIR] [--repo OWNER/REPO]` | Reports the hook's status. |
| `ink version` | Prints the commit revision. JSON includes `version` and `revision`. |

## Agent handoff and workspaces

| Command | Operation |
| --- | --- |
| `ink task claim --task TASK_ID` | Claims a cooperative task using the assigned identity and a durable local credential. |
| `ink task status --task TASK_ID` | Reads the task, delivery and verification status. |
| `ink task submit --task TASK_ID --handoff --request-id ID` | Submits completion evidence or a blocked handoff. Use `--new-sha`, `--review-id`, `--outcome` and `--reason` as appropriate. |
| `ink task retry --task TASK_ID` | Retries a blocked cooperative task using the original credential. |
| `ink notification status --event EVENT_ID` | Reads the notification and current merge status. |
| `ink notification ack --event EVENT_ID` | Claims an ACK. Only `first:true` represents a new receipt. |
| `ink context PR_URL --sha SHA [--before CURSOR]` | Reads paginated review context when policy and the exact task snapshot permit it. |
| `ink workspace list --repo DIR` | Lists worktrees and daemon allocations for a local checkout. |
| `ink workspace acquire --repo DIR --request-id ID` | Acquires a Full or Lite worktree. See `--kind`, `--ref`, `--branch` and `--task`. |
| `ink workspace release --id ID` | Releases a workspace after its safety checks pass. |
| `ink workspace remove --id ID` | Removes an allocation using the existing workspace safety checks. |

See [handoff](handoff.md) for the Agent protocol and [configuration](configuration.md) for policy and rollout details.

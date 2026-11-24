# Execution lifecycle and durable executor integration

## Identity and business state

A task gets a random, durable `task_id` when derived for a repository, PR, head
SHA, role and applicable review ID. A changed tuple or explicit reassignment
creates a new task identity. Repeating an identical reviewer assignment preserves
the existing task and allocated session. Retries retain the task ID and increment
`attempt`, generating a new `attempt_id` before delivery. Session preparation has its own durable reservation.

The current task contains `runs`; retired tasks with runs move into
`execution_history`. Each run stores the immutable request, destination, receipt,
local execution state, creation/end times, recovery reason, stop request time,
retry deadline and manual confirmation reason. `delivery` remains the transport
outcome for compatibility. GitHub observations alone drive the workflow `phase`.

All unresolved runs for the PR block subsequent bot deliveries, including runs
from older SHAs, reviewers, roles, Draft transitions and closed/reopened PRs.
This protects the PR resource; runtimes sharing one working directory across
*different* PRs must additionally serialize that directory themselves.

## Opt-in HTTP monitoring

Set `managed: true` on a [named Provider](adapters.md) only after implementing
the [authenticated admission and runtime guarantees](adapter-http-v1.md#managed-admission).
Delivery and monitoring use the persisted `task_id`, `attempt_id` and positive
`attempt` in `task`. Ink only sends status and stop to managed providers.

`status` and `stop` use the same configured endpoint, route, task and Bearer
credential as delivery. Remote endpoints require HTTPS and a token; redirects
are refused. The daemon pulls receipts from the configured executor, with no
public callback endpoint. A session ID alone cannot authenticate a receipt.
The executor must authenticate requests and isolate its ledger from other tenants.

Both operations return HTTP 2xx with a receipt:

```json
{
  "receipt": {
    "task_id": "...",
    "attempt_id": "...",
    "sequence": 2,
    "state": "running",
    "started_at": "2026-11-15T04:00:00Z",
    "heartbeat_at": "2026-11-15T04:00:10Z"
  }
}
```

States: `accepted`, `running`, `succeeded`, `failed`, `stopped`. `running` is the
start ACK and requires start/heartbeat timestamps. Success/failure also require
a start ACK and `finished_at`. A stopped task that never started may omit the
start/heartbeat timestamps. Every changed snapshot increments `sequence`;
exact duplicates are harmless, older/conflicting receipts are rejected. Start
time cannot change, heartbeat cannot move backwards and timestamps cannot be
more than 30 seconds ahead of daemon time. Keep executor clocks synchronized.

A terminal receipt is a runtime guarantee: the Agent and every tool have stopped,
no filesystem/GitHub side effect remains in flight, and delayed delivery cannot
restart that attempt. `stop` must persist a tombstone even when the original
request has not yet arrived. Unknown/missing records, an HTTP 404, session loss,
a missing PID, and a lease timeout do **not** constitute a terminal guarantee.
Return an error or a nonterminal receipt when termination cannot be established.

## Durable executor ledger

`internal/execution/ledger` supplies a SQLite implementation for in-repository
HTTP executor integrations. It is a building block; configuring an arbitrary
HTTP server does not automatically install it in that server. Integrations in other modules implement equivalent semantics through the HTTP contract.

The normal executor flow is:

1. `Accept(task)` durably records admission before replying `accepted`. Exact
   duplicate deliveries return the existing receipt. Changed payloads with the
   same identity conflict. Older attempt numbers and other active tasks for the
   same repository/PR cannot acquire admission.
2. `Start(task)` atomically claims a fresh runtime owner and records the start
   ACK. Only the winner may spawn/activate the agent. Never start side effects
   before ownership is durable. Repeated delivery or a restarted process cannot
   claim an already running attempt.
3. `Heartbeat(task, owner)` updates the receipt while that owner runs. The runtime
   must enforce this ownership at its tool boundary as well as in the ledger.
4. Join the agent and all tools, then `Finish(task, owner, "succeeded"|"failed")`.
5. Serve `status` with `Status(task)`. Serve `stop` with
   `Stop(ctx, task, stopAndJoin)`. The callback must terminate and join the actual
   runtime identified by its durable owner, including any pending spawn. The
   ledger serializes Start/Stop and clears ownership only after confirmation.
   It rejects subsequent heartbeats/finish calls from the old owner.

A running record survives executor restart. If the new process cannot prove the
old runtime has stopped, its stop callback returns an error and the record stays
active. For an accepted task that never started, Stop can safely persist a
terminal tombstone without a runtime callback. Database transitions are serialized
across connections/processes; this is not merely daemon memory deduplication.

The ledger cannot stop an arbitrary agent, revoke its credentials, or intercept
raw filesystem operations itself. The runtime must implement cancellation and
join all tools (or enforce equivalent access revocation). Merely rejecting an
old ACK is insufficient. Do not enable monitoring when this contract is unmet.
The Stop callback runs under the ledger transaction: it must not call the ledger
or wait for a worker's Finish/Heartbeat call. Cancel/join the worker first; those
late ledger writes will fail after Stop commits. Bound the callback by context.

For conversation creation, call `ReserveSession(task_id)` before the external
create operation. Only a fresh reservation may create a conversation. Record its
result with `SetSession`. A reservation left unfinished by a crash returns
`ErrUnknown` after restart and must be reconciled with the external service; it
never authorizes creating another thread. Preparation must not start an agent.

## Recovery and operator handling

The daemon checks executors before GitHub polling. ACK, heartbeat and total run
deadlines are configured in seconds under `execution` (defaults: 120/180/3600).
At timeout, or after task retirement, it records a stop request before asking the
executor to stop. Replacement waits for terminal confirmation. Failure/confirmed
stop retries use `backoff_seconds * 2^(attempt-1)`, capped at one hour, with
`max_attempts` total attempts (defaults 30 seconds and 3 attempts). A value of 1
disables automatic retries. Poll cadence bounds when deadlines are noticed.
The retry deadline and previous runs survive daemon restart. Exhaustion is
reported on the current task and requires operator action.

If stop is unconfirmed, the run remains `unknown` and blocks automatic and manual
retry. Stop the old runtime and prevent all queued/delayed requests from reviving
it. Reconcile any ambiguous conversation allocation. Then, through the private
Unix socket, POST to `/v1/work/OWNER/REPO/NUMBER/confirm-stopped`:

```json
{"task_id":"...","attempt_id":"...","stopped":true,"reason":"Stopped and joined runtime; fenced queued delivery; reconciled existing thread"}
```

This is an auditable operator assertion, not a process-kill API. It can target
retired tasks. Wrong identities are rejected; no broad override exists. The
current failed/stopped task will enter backoff if attempts remain. An explicit
`retry` may bypass backoff or retry after exhaustion, but never bypass unresolved
executions. Successful runs do not advance GitHub state or automatically rerun.

cc-connect's webhook receives task and attempt IDs for diagnostics. Ambiguous
delivery through a provider without status and stop support requires explicit
operator confirmation before retry. The v6 database records new attempts before
delivery. Follow the [single-instance checklist](migration.md) to switch from
an earlier schema.

## Review waiting and diagnostics

The work API persists `workflow` and `alerts` independently of agent turns.
Transport acceptance maps to `waiting_start`, not `executing`; execution requires
a validated running receipt. GitHub observations still determine `phase`.
Only the assigned reviewer's current-SHA formal review advances the workflow.
A mismatched identity or a newly submitted stale-SHA result is diagnosed without
weakening that filter. Old reviews preceding the current task are not stale-result
alarms. Diagnostics have stable IDs, survive restart, log once on creation and
remain as inactive history after resolution.

Review deliveries carry `github_login`, optional `github_command` (repository
`reviewer_command` or CLI `--github-command`) and active diagnostic feedback.
The reviewer is instructed to verify its identity and report a blocker instead
of falling back to another account. Developer deliveries explicitly request
checks and a pushed fix so the existing head-change observer can trigger review.

If a managed execution succeeds and there is still no formal review or new SHA,
`execution.result_seconds` (default 120) controls when a diagnostic appears.
The diagnostic does not replay the successful attempt. An unmanaged adapter's
`accepted` response confirms transport only. Ambiguous executions remain
subject to the stop-confirmation rules above.

## Cooperative business handoff

`task_mode: "cooperative"` selects a separate claim/submit workflow for newly created tasks. A task wake does not create a runtime attempt and does not authorize side effects before claim. Explicit business handoff is not a terminal Monitor receipt. Legacy/managed unresolved attempts remain blocking and must be reconciled before migration. See [claim, verification, retries and trust boundaries](handoff.md).

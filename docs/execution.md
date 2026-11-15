# Execution lifecycle and durable executor integration

## Identity and business state

A task gets a random, durable `task_id` when derived for a repository, PR, head
SHA, role and applicable review ID. A changed tuple or explicit reassignment
creates a new task identity. Repeating an identical reviewer assignment preserves
the existing task and allocated session. Retries retain the task ID and increment
`attempt`, generating a new `attempt_id` before any external request.

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

Legacy `prepare` and `deliver` retain `version: 1` and their response format.
Requests additionally contain `task_id`, `attempt_id` and positive `attempt` in
`task`. Preparation and delivery use the same identity. Set
`http_provider_execution: true` only after implementing the following operations.
Without this opt-in no status/stop POSTs are sent to a legacy endpoint.

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

States: `accepted`, `queued`, `running`, `succeeded`, `failed`, `stopped`. `queued`
requires a heartbeat and no start/end timestamps; its queue deadline is distinct
from the start ACK deadline. `running` is the
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
HTTP server does not automatically install it in that server. Integrations in
other Go modules must port/export this helper or implement equivalent semantics.

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

Existing tasks with evidence of prior delivery but no run records get synthetic
legacy runs with stable persisted IDs. This includes accepted/unknown deliveries
and any task with `attempts > 0`, including `pending` tasks left by the old retry
API. They migrate to `unknown`; genuinely unattempted pending tasks remain ready
for first delivery. Legacy attempt numbers are normalized to at least one. Since
the executor never received these IDs, only operator confirmation can resolve
them. The legacy cc-connect webhook retains conservative behavior. The opt-in managed
endpoint described below supplies durable admission, queueing and runtime
termination; it requires the companion cc-connect change.

The new fields live in the existing SQLite work JSON, so the storage schema stays
at version 1. Back up before upgrading. Older binaries do not enforce execution
ownership and may discard the added fields when saving: do not run an older
daemon against active new-version executions. Stop/fence them first and retain
a backup containing the execution history before using the documented rollback.


## cc-connect managed integration

Upgrade both repositories before enabling this mode. Existing unmanaged attempts
must be stopped and fenced manually; toggling the flag cannot retrofit IDs into
an old webhook request. Do not point a managed provider at an unmodified server.

In cc-connect:

```toml
[webhook]
enabled = true
port = 9111
path = "/hook"
token = "<inject the shared secret securely>"
execution_state = "/path/to/private/ink-executions.json"
```

The state directory must have mode 0700; state/lock files use 0600. This inbox is
opt-in, uses an OS-held exclusive lock plus fsync/rename persistence, and is
separate from ordinary session history. Locking is supported on Linux, macOS and
FreeBSD. Legacy `/hook` requests retain their original behavior. Managed requests
require a Bearer header even if the legacy endpoint accepts other authentication.

In Ink Commons (credentials continue to come from environment):

```json
{
  "cc_connect_url": "http://127.0.0.1:9111/hook",
  "cc_connect_execution": true,
  "execution": {
    "queue_seconds": 3600,
    "result_seconds": 120,
    "max_attempts": 3
  },
  "repositories": [{
    "repo": "owner/repository",
    "reviewer_login": "reviewer-app[bot]",
    "reviewer_command": "reviewer-cli"
  }]
}
```

The adapter sends version-1 `deliver`, `status`, `stop`, `resolve` and `notify`
operations to `/hook/execution` using the HTTP lifecycle envelope plus `prompt`.
The entire task and immutable route identify a round. Duplicate requests reuse
the existing record; changed payloads/targets with the same ID conflict. A stop
for an unseen round creates a durable tombstone. The same PR cannot acquire a
new round while an old one is queued, running or unresolved. Different PRs can
queue on one busy conversation in admission order.

Queue admission is not a start ACK. The executor persists a dispatch intent before
creating/resuming the runtime, and publishes `running` only after its Send call
accepts the prompt. It retains the session fence until the runtime's explicit
termination barrier returns and the terminal receipt is durably committed.
Ordinary /new, /switch and stale-lock recovery cannot release this ownership.
A restart during runtime creation/execution leaves an orphaned record and
quarantines its conversation; it is not silently replayed. The status endpoint
exposes the reason. An operator must stop/join the original processes and use
`confirm-stopped`; the daemon first sends `resolve` to the executor, so its
ownership ledger is reconciled too. This is an explicit operator assertion.

Only adapters implementing confirmed termination can receive managed tasks.
Codex tracks and terminates its managed process groups; Pi RPC kills/joins its
RPC process group and makes concurrent closes idempotent. Pi JSON and other
unadapted runtimes are rejected before admission. Managed tasks must not escape
the runtime's process groups or leave detached external workloads. Workspace
isolation across different PRs remains a separate deployment responsibility.

To avoid dropping a user's message in the final-response/termination gap, new
ordinary turns are refused while a managed round owns the conversation. Status,
history, stop, permission callbacks and /ps remain available. The durable *work*
queue continues to accept follow-up tasks while the conversation is busy.

### Workflow results and diagnostics

`workflow` records the waiting/attention state independently of the agent turn;
`phase` continues to follow only the expected reviewer's current-SHA formal
reviews and GitHub head changes. `alerts` persist diagnostic IDs, activity,
creation time, notification acceptance and delivery errors. Unrelated reviewers'
comments never authorize code changes.

Expected login/CLI and diagnostic feedback travel in every review task. A wrong
identity or a newly submitted stale-SHA review raises a diagnostic without
weakening the reviewer filter. Historical reviews from before the new head are
normal and do not generate stale-result alarms. A succeeded review turn that
produces no valid result after `result_seconds` schedules another attempt using
the same stable task ID, a new attempt ID and capped backoff. `max_attempts`
bounds these follow-ups. Developer completion without a new SHA raises a blocker
instead of repeatedly editing the same working directory. No automatic merge is
performed.

Alerts are sent to the development conversation via a durable notification
outbox. cc-connect retries failed platform sends and deduplicates accepted notice
IDs. Delivery is at least once: a crash between a platform send and the local
commit can repeat a diagnostic, but cannot launch another coding task. API/log
diagnostics remain available if the provider lacks notification support.

Thread creation reserves the current task in SQLite *before* contacting Discord.
An interrupted or failed allocation remains reserved/unknown and blocks another
create, even after the old execution attempt is stopped. Find the existing
thread, then POST `confirm-prepared` with `task_id`, an existing-session `route`
and a nonempty `reason`. The provider/target must match the reserved destination.
The operator reason is retained, and retries reuse that session.

### Cross-repository verification

The fixture in cc-connect mocks only the agent process, GitHub and platform I/O;
it uses the real durable inbox, engine, session locks and authenticated HTTP API.
Build it from the companion checkout, then run the tagged integration test here:

```sh
(cd /path/to/cc-connect && go build -o /tmp/ink-cc-fixture ./tests/ink-managed-execution)
INK_CC_CONNECT_FIXTURE=/tmp/ink-cc-fixture go test -race -tags ccconnect_integration ./internal/ink -run '^TestCCConnectFeedbackLoop$' -count=1
```

The test submits a wrong-identity review, restarts the daemon while it waits,
checks automatic reviewer follow-up, wakes a busy developer through the durable
queue, advances to a new SHA, and verifies current-SHA approval with exactly one
developer execution. It also verifies that identity/CLI and previous diagnostics
cross the real transport boundary. On macOS, existing cc-connect fixture tests
may require a canonical TMPDIR such as `/private/tmp` to avoid symlink aliases.

# Adapter HTTP v1 contract

Ink sends JSON POST requests to the configured endpoint of a named Adapter.
The provider name is a persistent route identifier. A route contains
`provider`, `target`, and optionally `session` and `parent`. Provider-specific
identifiers are opaque to the core. A resolved session is required for dispatch.

The request envelope contains `version: 1`, `operation`, and `route`. Task
operations include `task` with the repository, PR, role, exact head SHA and task
ID. Execution attempts also include `attempt_id` and a positive `attempt`.
All retries for an attempt use the same identity and immutable payload.
The existing HTTP endpoint already uses this envelope for prepare, deliver,
status and stop. The remaining operations are the contract for the Adapter
implementation in Task 3.

## Capabilities

Each named Adapter has an explicit capability set. Ink checks the capabilities
required by the selected route and workflow before issuing a side effect. An
unsupported required capability is a configuration or task error. An Adapter
receiving an unsupported operation returns an explicit error. Upgrading a
legacy connection must not probe it by sending new operation types.

| Capability / operation | When required | Request and success result |
| --- | --- | --- |
| `prepare` | A route needs a new session | Task and route in, resolved `session` out. Allocation recovery uses the same persisted reservation. Preparation never starts an agent. |
| `deliver` | Direct execution | Task and resolved route in, `status: "accepted"` out after admission. Acceptance is a transport result. |
| `wake` | Cooperative dispatch | Task and resolved route in, `status: "accepted"` out. The recipient must claim before acting. Repeated wakes for the same task can only invite the same claim. |
| `notify` | A notification is routed through the Adapter | `notification` includes `event_id`, event type, PR, SHA and event details. The envelope carries the frozen route and `require_ack`. `status: "accepted"` records transport acceptance. |
| `blocked_notice` | A blocked-task alert is routed through the Adapter | Task, destination and `reason` in, `status: "accepted"` out. This is an informational alert and creates no executable work. |
| `status` + `stop` | Managed direct execution | Frozen route and original task/attempt in, `receipt` out. Both operations are required as a pair. |
| `placement` | The selected policy requires placement resolution | Read-only task and route lookup returns the effective `parent` for allocation or an existing `session` for reuse. The core validates the result against the selected policy. |
| `conversation_context` | A task requests access to conversation history under an enabled context policy | Authorized source route, task with frozen cutoff and optional `before` cursor in. Return bounded pages with source session, messages and an optional `next_before` cursor, oldest message first within each page. |

## Placement, notifications and recovery

- Ink validates explicit parent, same-channel and reuse policies against the
  development conversation and persists the resolved destination before it
  wakes or delivers work. If placement is unavailable or inconsistent, the
  task surfaces an error and stays undispatched. It may never silently choose
  another channel or use a default destination.
- Before creating a conversation, persist an allocation identity bound to the
  task and its resolved parent. For a placement-aware Adapter, `prepare`
  carries that identity in `allocation` and a `mode` of `create` or `find`.
  Create may start a new allocation only for a newly reserved identity. If the
  request might have taken effect, including a timeout or restart mid-request,
  recovery uses `find` exclusively. Find can adopt a unique match for the
  reservation and parent. Missing or multiple matches leave the allocation
  unresolved and produce no wake, fallback or second create. A legacy prepare
  with an ambiguous outcome and no lookup capability stays blocked for
  reconciliation.
- Ink persists the notification event ID, destination and ACK requirement
  (`RequireACK`) before delivery. Retries retain those values across restart.
  A changed developer route supersedes the old event. When `require_ack: true`,
  the Adapter must relay the same ID and instruct the recipient to ACK through
  Ink. A successful POST leaves
  the event awaiting receipt. Duplicate ACKs return `first: false` and stop
  redelivery for that event. Notification payloads convey information only.

## Trust and managed admission

Ink owns repository and route policy, GitHub business state, task and attempt
identity, head SHA, the frozen context cutoff and runtime authorization.
Context access must pass the core's current policy checks and match the
authorized task, SHA, source and cutoff. Ink enforces the cutoff and bounded
pagination on returned data. Conversation content is untrusted input.

The Adapter authenticates Ink's outbound requests and isolates tenant data and
runtime ownership. Remote endpoints require HTTPS and a Bearer token. Ink
bounds request and response handling and refuses redirects. Agent `claim`,
`handoff` (task `submit`) and notification `ack` stay on the local Unix socket,
under the daemon UID's filesystem permissions (private state directory and
socket). Keep that socket and claim credentials out of the Adapter. An Adapter
response never substitutes for a claim, a handoff, an ACK or GitHub evidence.

`managed: true` is explicit opt-in. The Adapter must durably fence duplicate
and delayed deliveries, stop and join the actual agent and all its tools, and
persist a stop tombstone even when the original request has yet to arrive.
Status and stop must return authenticated, task/attempt-bound, monotonically
sequenced receipts. Unknown ownership, request timeouts and missing records
cannot establish termination. Ink retains unresolved attempts and holds
replacement until the existing attempt is proven terminal or an authorized
operator reconciles it. See [execution lifecycle and receipt rules](execution.md)
and [cooperative handoff](handoff.md).

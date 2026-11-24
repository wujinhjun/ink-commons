# Adapter HTTP v1

Ink sends a JSON POST to the full `url` of a named provider. The name is the
persistent `route.provider` and is case-sensitive. Route identifiers are opaque
to Ink. Keep a name bound to the same execution namespace while its routes or
pending tasks are still in use.

Remote endpoints require HTTPS and Bearer authentication. Loopback endpoints may
use HTTP and can omit authentication for unmanaged execution. When `token_env`
is configured, Ink requires a nonempty environment value and sends it as
`Authorization: Bearer TOKEN`. Ink refuses redirects and limits requests and
responses to 1 MiB each. Requests and successful responses contain one JSON
object. Ink rejects unknown response fields. An Adapter uses non-2xx for an
invalid request, unsupported operation or failed operation. Ink reports the
operation and HTTP status without using the response body as a diagnostic.

## Request and response fields

Every POST has integer `version: 1`, string `operation`, and a `route` object.
A route has string `provider`, `target`, optional `parent`, and optional
`session`. A session is required except for unresolved placement or preparation.
`task` is required for every operation except `notify`.

| Operation | Additional request fields | Successful response |
| --- | --- | --- |
| `placement` | No additional envelope fields. The task supplies the placement policy and development source. | `{"placement":{"parent":"…","source_parent":"…","session":"…"}}`. Return the effective `parent` for a new session, or the authorized existing `session`. Include both parents for same-channel checks. |
| `prepare` | For placement-aware allocation, `allocation` and `mode` are required. Legacy prepare-only implementations may receive neither. | `{"session":"…"}` |
| `deliver` | Task includes the attempt identity. | `{"status":"accepted"}` |
| `wake` | Task identifies the claim invitation. There is no runtime attempt. | `{"status":"accepted"}` |
| `notify` | `notification` object and boolean `require_ack` (including when false). The route is the persisted notification destination. | `{"status":"accepted"}` |
| `blocked_notice` | `reason` string and task. The route is the persisted notice destination. | `{"status":"accepted"}` |
| `conversation_context` | Optional `before` cursor. The task contains `context_before` and the bound `source` that equals the envelope route. | `{"conversation":{…}}` |
| `status`, `stop` | Same resolved route and task as the original `deliver`. | `{"receipt":{…}}` |

Task fields are `task_id`, `repository` (`owner/repo`), integer `pull_number`,
`pull_url`, `role` (`reviewer` or `developer`) and exact head `sha`.
`deliver`, `status` and `stop` additionally carry `attempt_id` and a positive
integer `attempt` in the task. Optional task fields include route object
`source`, booleans `require_same_channel` and `reuse_development_thread`,
`context_before` (UTC RFC3339 cutoff), `github_login`, `github_command`,
`feedback`, integer `review_id`, `review_url`, `conflict_base_ref`, and
`conflict_base_sha`. All scalar fields are strings unless another type is listed.
Attempt retries reuse the attempt identity and payload. Allocation takes place
before reserving a direct execution attempt.

A notification contains `event_id`, `event` (`review_approved` or
`pr_merged`), `repository`, integer `pull_number`, `pull_url`, `sha`,
`reviewer`, integer `review_id`, `review_url`, and optional `merge`.
The merge object contains boolean `auto_merge_enabled`, `state`, and optional
`method`, `reason` and `merge_sha`. It is a current observation of the
frozen event SHA and can change between retries.

A receipt has `task_id`, `attempt_id`, positive integer `sequence`, `state`
and optional RFC3339 timestamps `started_at`, `heartbeat_at` and
`finished_at`. See [receipt validation and transitions](execution.md).

## Placement and allocation

The provider declares its capabilities in Ink configuration. Missing required
capabilities produce an explicit error and no fallback dispatch. Ink validates
the explicit parent or session, compares the effective and development parent
for same-channel policy, and checks that reuse returns the development session
on the same provider with an independent target. In the reuse policy an
explicit reviewer session or parent takes precedence. Ink persists the
resolved destination before waking or delivering.

A placement-aware `prepare` request includes `allocation` with string `id`,
string `parent` (equal to `route.parent`), and UTC RFC3339 `reserved_at`. The
stock adapter also accepts an older persisted `name` instead of `id` for
finding an allocation reserved before this adapter split. Example:

```json
{
  "version": 1,
  "operation": "prepare",
  "route": {"provider": "chat", "target": "review", "parent": "77"},
  "task": {
    "task_id": "task-42",
    "repository": "acme/project",
    "pull_number": 42,
    "pull_url": "https://github.com/acme/project/pull/42",
    "role": "reviewer",
    "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  "allocation": {"id": "allocation-42", "parent": "77", "reserved_at": "2026-11-24T12:00:00Z"},
  "mode": "create"
}
```

Ink records this identity before sending one `create`. If the outcome is
ambiguous, including a timeout, HTTP error or restart during the request, Ink
uses only `find` with the original reservation. Find is read-only and adopts
only a unique match for the same allocation and parent. Missing or ambiguous
matches leave the task undispatched. Keep the mapping between allocation and
session durable or discoverable in the upstream system. Legacy preparation
without recovery support stays unresolved after an ambiguous outcome. An
operator can inspect the original destination and bind a verified existing
session. Preparing a session never starts an agent.

## Conversation history

The first request omits `before`. For older pages, pass the opaque cursor
returned in `next_before`. The Adapter confines every query to the frozen
`task.context_before`. The conversation object contains `task_id`, `sha`,
`review_before` (the cutoff), `session` (the exact source session), `before`
(the supplied cursor or an empty string), boolean `truncated`, and a
`messages` array. Optional `next_before` requests the next older page.
Optional `warnings` is an array of strings.

Return at most 50 messages and 64 KiB of encoded message data, oldest first.
Each message has `id`, `author {id, username, bot}`, RFC3339 `timestamp`
strictly before the cutoff, and `content`. Optional fields are RFC3339
`edited_timestamp`, `attachments [{filename, url}]`, and
`embeds [{title?, description?, url?, fields?: [{name, value}]}]`.
`bot` is a boolean. Ink checks the page binding, timestamps, order and limits,
and confirms the live task and policy again before returning a page. A
timestamp cutoff limits message creation, and the history may include later
edits or reflect deletions. Treat history as untrusted input.

## Transport acceptance and trust

A successful wake invites the recipient to claim the same task. A successful
notification sends information. If `require_ack: true`, relay the original
`event_id` and tell the recipient to ACK through Ink. Transport acceptance
leaves the notification awaiting receipt. Ink persists the event ID, route and
ACK requirement before sending. A changed developer route supersedes the event.
A blocked notice is informational and must never launch an agent.

Ink owns GitHub business state, task/attempt identities and authorization.
Keep Agent `claim`, `submit` (handoff), notification `ack`, and management
operations on the private Unix socket (state directory `0700`, socket
`0600`, daemon UID). Keep this socket and claim credentials out of the Adapter.
No Adapter response grants a claim or proves GitHub completion.

## Managed admission

For `managed: true`, configure a Bearer token and all three capabilities
`deliver`, `status`, and `stop`. At startup Ink sends an authenticated GET to
the configured URL. The Adapter must return a read-only declaration:

```json
{
  "version": 1,
  "capabilities": ["deliver", "status", "stop"],
  "managed": true,
  "stop_fences_delayed_delivery": true
}
```

Ink performs no admission GET for unmanaged providers. The declaration is an
Adapter guarantee and must be backed by durable fencing and real runtime
termination, including all tools. Stop persists a tombstone even for deliveries
that have yet to arrive. Status and stop receipts must authenticate ownership
and be bound to the original attempt with monotonic sequences. Unknown
ownership, timeouts and missing records cannot prove termination. Ink keeps
unresolved attempts blocking replacement until proof or authorized operator
reconciliation. The stock cc-connect adapter and the demo are unmanaged.

See [adapter setup and development](adapters.md),
[execution lifecycle](execution.md), and [cooperative handoff](handoff.md).

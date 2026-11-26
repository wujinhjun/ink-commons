# GitHub credential and merge contracts

## TokenProvider

`TokenProvider` is an internal credential source for GitHub requests. Its
logical operation is `Token(ctx, scope) -> (token, expiry, error)`, where scope
identifies the GitHub host, repository, role and configured credential identity.
A provider may use an explicitly configured PAT, a GitHub App installation or an
explicit credential helper. All three follow the same role boundary.

| Role | Authorized use |
| --- | --- |
| `reader` | Repository and PR observations, reviews, checks, and repo-scoped GraphQL conflict diagnosis. Use fresh reads for merge preflight. |
| `merger` | Submit GitHub merge mutations and query the Stack merge requests started by that identity. Use repository-scoped credentials with the permissions required for those operations. |

Cache by host, repository, role and credential identity. Scope the App token
request to that repository and role, refresh expiring credentials before use,
and invalidate affected entries when credentials or scopes change. Keep token
material out of logs, workflow payloads and persisted work state. An unavailable
explicit credential fails the request. Implicit fallback to a personal `gh`
login, ambient helper or the other role is forbidden. A shared App identity is
possible only with the existing explicit opt-in and still uses separate role
scopes and cache entries. The Agent's formal GitHub review identity is separate
from both daemon roles.

## MergeStrategy and Merger

- `MergeStrategy` selects how to submit and reconcile a merge that the core
  has already authorized. Its input is a frozen repo/PR, expected head SHA,
  merge method, stack state and identity reference. It reports a completed,
  pending, rejected or uncertain result with the evidence it has, including
  any remote request ID. Strategy selection never grants merge permission.
- `Merger` supplies the GitHub merge operations to the strategy through the
  configured merger identity. Submission requires the expected SHA. A pending
  Stack lookup takes the original request ID and original credential identity
  reference, even after policy changes or daemon restart. If that identity is
  unavailable, retain the request for reconciliation. Persist references and
  evidence only.
- The core owns policy and persistence. Merging requires explicit opt-in,
  verified handoff and approval as applicable, and a fresh check of PR state,
  head, stack membership and configured checks. Persist the intent before a
  write and the pending request ID as soon as it is known. Reconcile uncertain
  submissions with GitHub without blindly issuing another write. Keep
  `post_approval` as an independent side effect.

The v0.1 `direct` strategy keeps both existing SHA-locked paths. Ordinary PRs
use synchronous direct merge. Eligible Stack PRs use asynchronous direct merge
and poll the stored request until reconciled. A successor waits for retargeting
and approval of its rebased head where applicable. Neither path bypasses rules.
A v0.2+ `github_auto_merge` strategy can substitute native GitHub submission and
reconciliation while preserving role isolation, the SHA-bound policy boundary
and GitHub as the authority for business completion.

## Daemon credential configuration

Configure the static JSON keys `github_reader` and `github_merger`. Each object
selects one source:

- PAT: `{"token_env":"INK_READER_PAT"}` reads the named daemon environment
  variable. Use a different credential for the merger. Existing
  `INK_GITHUB_TOKEN` deployments still work for reads when no reader source
  is configured. With no reader source, public observations can use anonymous
  HTTP.
- App: `{"app":{"app_id":123,"installation_id":456,"private_key_file":"/run/secrets/github-app.pem"}}`
  reads a private RSA PEM file with mode `0600` or stricter. The reader token
  requests read access to contents, pull requests, and checks. The merger
  token requests write access to contents and pull requests. Both tokens are
  restricted to the requested repository. An App serving both roles needs
  explicit repository opt-in via `auto_merge.reuse_reader_identity`.
- Helper: `{"helper":{"command":"/usr/local/bin/ink-credential","args":[],"identity":"reader-app"}}`
  runs the absolute executable with the configured arguments followed by
  `get`. Standard input carries Git credential protocol fields `protocol`,
  `host`, and `path` (the repository), and the process receives
  `INK_GITHUB_ROLE`. Standard output can be a single token line, Git
  credential format with `password` and optional `password_expiry_utc`, or
  JSON with `token` and optional RFC 3339 `expires_at`. A helper without an
  expiry is queried again after about 30 seconds. Use a distinct `identity`
  for each principal and change it when a helper switches principals. Scope
  helper tokens to the repository and the requested role.

Legacy `github_command` and pinned `github_merger_command` wrappers continue to
work with their existing argument and HTTP response protocol. Configure one
source per role. Commands run without a shell, with isolated Git and GitHub CLI
configuration and without ambient GitHub CLI tokens or configured PAT variables.
The Agent's formal review command remains separate.

A pending Stack merge retains an opaque reference to the credential that
submitted it. A replacement PAT or wrapper cannot query that request. Restore
the submitting credential to resume lookup. App signing-key rotation preserves
the installation identity. For records predating identity references, Ink
retains the request and reconciles it against the observed PR outcome.

## Conditional observations

Polling uses conditional GETs for authenticated open PR lists, PR details, and
review pages. Each cache entry keeps the complete URL, the ETag, the response
body, and its pagination links in memory. The key also includes the reader's
credential scope. A 304 is reconciled locally with the stored page and links.
Anonymous reads and the legacy HTTP wrapper use full responses because those
paths do not expose a token scope to the cache. Checks, conflict diagnosis,
handoff verification, and merge preflight use fresh requests.

Secondary rate limits pause both GitHub roles for the server's `Retry-After`
interval or for exponential backoff when the header is absent. A primary
rate limit cools down the affected role until GitHub's reset time.

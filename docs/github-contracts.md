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
and GitHub as the authority for business completion. Task 4 implements the
interfaces against the existing behavior.

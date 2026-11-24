# Minimal HTTP executor

This standard-library example implements `prepare` and `deliver` from the
[Adapter HTTP v1 contract](../../docs/adapter-http-v1.md). Preparation derives
a stable demo session. Delivery logs acceptance and performs no work.

From the repository root:

```sh
go run ./examples/http-executor
```

Add the following settings to the static config of an Ink daemon with GitHub
access. If the repository is already registered, set its `reviewer_route`
through `ink config` instead of using static seed values.

```json
{
  "providers": {
    "local-demo": {
      "url": "http://127.0.0.1:8789/v1/execute",
      "capabilities": ["prepare", "deliver"]
    }
  },
  "repositories": [{
    "repo": "owner/repo",
    "reviewer_login": "reviewer",
    "reviewer_route": {"provider": "local-demo", "target": "reviewer"},
    "task_mode": "legacy"
  }]
}
```

Link an open PR to the daemon and make it Ready for review. The demo logs
`demo delivery accepted`. The local work item records the `demo-…` session
and the delivery attempt. GitHub review state remains pending until the assigned
reviewer submits a formal review.

To verify without GitHub credentials or an agent:

```sh
go test -v ./examples/http-executor
```

The test starts an isolated Ink daemon with a fake GitHub reader, links the PR
through the real Unix socket, exercises both operations over loopback HTTP, and
checks the persisted route, SHA, and delivery state.

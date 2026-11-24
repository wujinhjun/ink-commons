# Configure and run HTTP adapters

Add each adapter to the daemon's static `providers` map. Use its name in a
repository policy, PR assignment or developer session. Multiple named providers
can point to separate processes and use different credentials:

```json
{
  "providers": {
    "chat": {
      "url": "http://127.0.0.1:8788/v1/execute",
      "token_env": "CHAT_ADAPTER_TOKEN",
      "capabilities": ["placement", "prepare", "deliver", "wake", "notify", "blocked_notice", "conversation_context"],
      "require_ack": true
    },
    "local-demo": {
      "url": "http://127.0.0.1:8789/v1/execute",
      "capabilities": ["prepare", "deliver"]
    }
  }
}
```

With an active daemon, select a route using the same name:

```sh
ink config set reviewer_route '{"provider":"chat","target":"reviewer-project"}' --repo owner/repo
ink review --github-login 'reviewer[bot]' --provider chat --target reviewer-project PR_URL
```

Set `task_mode` to `cooperative` for claim-based workflows. Configure `wake`
on the recipient's provider, or `deliver` for direct execution. Existing
developer sessions used by the Git hook set `INK_PROVIDER`, `INK_TARGET`
and `INK_SESSION`. A runtime that supplies its own environment variable names
can translate them in the launch or hook environment. The claim command in
a cc-connect wake carries the route flags explicitly.

## Run the cc-connect adapter

Build and run the independent entry point on Linux or macOS:

```sh
go build -o ink-adapter-ccconnect ./cmd/ink-adapter-ccconnect
./ink-adapter-ccconnect --provider chat --listen 127.0.0.1:8788
```

Inject `INK_ADAPTER_TOKEN` into the adapter process and the same value into
`CHAT_ADAPTER_TOKEN` in the example Ink daemon. The adapter process also needs
`INK_CC_CONNECT_URL` (the loopback HTTP webhook) and `INK_CC_CONNECT_TOKEN`
(the webhook's Bearer token). Give the adapter `INK_DISCORD_TOKEN` for Discord
placement, creation, history, and blocked notices. Declare only the capabilities
supported by its credentials and target runtime. Keep the Ink socket and state
directory under the service UID and keep its claim credentials out of the
adapter's execution environment.

`target` is a cc-connect project. `session` uses `discord:CHANNEL_ID` (an
optional additional `:USER_ID` is preserved), and `parent` is a Discord
text-channel ID. Without a parent, new review placement follows a linked
public development thread or text channel. For automatic reuse and history,
the development source must be a public thread under an ordinary text channel.
Discord permissions must allow the corresponding API calls. Use
`require_ack: true` to relay approval and merge receipt instructions through
cc-connect. Its webhook confirms transport only. Leave `managed` off.

## Implement an adapter

Start with the [standalone HTTP demo](../examples/http-executor/README.md) and
the [wire contract](adapter-http-v1.md). Declare the operations you implement
and choose a unique provider name. Treat preparation, wake, direct execution
and informational notifications as separate operations. Implement the
durability and termination guarantees before enabling managed operation.
The repository's SQLite executor ledger is internal to Ink Commons.

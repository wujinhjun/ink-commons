## Summary

Describe what this pull request changes and why.

## Related Issues

Closes #

## Verification

- [ ] `make build`
- [ ] `make test-race`
- [ ] `make vet`
- [ ] `make lint`
- [ ] `make sqlc-check`
- [ ] Documentation and `config.schema.json` / `config.example.json` updated (if applicable)

## Security & Privacy Checklist

- [ ] No secrets, tokens, private keys, or personal identifiers/paths added
- [ ] Core (`cmd/ink`, `internal/ink`) does not import `internal/execution/ccconnect`
- [ ] External side effects remain persisted before execution and respect existing trust boundaries

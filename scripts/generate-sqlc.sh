#!/bin/sh
# Run from the repository root; generated Go files are committed.
set -eu
version=v1.30.0
if command -v sqlc >/dev/null 2>&1 && [ "$(sqlc version)" = "$version" ]; then
  exec sqlc generate
fi
exec go run github.com/sqlc-dev/sqlc/cmd/sqlc@"$version" generate

GO ?= go
GOLANGCI_LINT ?= golangci-lint
REVISION ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf development)
VERSION ?= development
INK_PACKAGE := $(shell sed -n 's/^module //p' go.mod)/internal/ink

.PHONY: build test test-race vet lint sqlc-check clean
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags "-X $(INK_PACKAGE).BuildRevision=$(REVISION) -X $(INK_PACKAGE).BuildVersion=$(VERSION)" -o bin/ink ./cmd/ink
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags "-X main.buildRevision=$(REVISION) -X main.buildVersion=$(VERSION)" -o bin/ink-adapter-ccconnect ./cmd/ink-adapter-ccconnect

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) run

sqlc-check:
	./scripts/generate-sqlc.sh
	@status=$$(git status --porcelain --untracked-files=all -- internal/ink/statedb internal/execution/ledger/ledgerdb) && \
		if [ -n "$$status" ]; then printf '%s\n' "$$status"; exit 1; fi

clean:
	rm -rf bin dist

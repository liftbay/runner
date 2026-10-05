VERSION ?= dev
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)

.PHONY: build test release

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/liftbay-runner ./cmd/liftbay-runner

test:
	go vet ./... && go test -race ./...

release:
	scripts/release.sh $(VERSION)

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
SERVER_TARGETS := linux/amd64 linux/arm64 darwin/arm64 windows/amd64

.PHONY: build build-server run-local test dist dist-server clean

build:
	go build -ldflags "$(LDFLAGS)" -o dist/device-agent ./cmd/agent

build-server:
	go build -ldflags "$(LDFLAGS)" -o dist/device-hub ./cmd/server

# Build and run server + agent on this machine (Ctrl+C stops both).
run-local:
	./scripts/run-local.sh

test:
	go vet ./... && go test -race ./...

# Cross-compile release binaries (no cgo, so no toolchains needed).
dist:
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" \
			-o dist/device-agent-$$os-$$arch$$ext ./cmd/agent || exit 1; \
	done

dist-server:
	@for t in $(SERVER_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building server $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" \
			-o dist/device-hub-$$os-$$arch$$ext ./cmd/server || exit 1; \
	done

clean:
	rm -rf dist

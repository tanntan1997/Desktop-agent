VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build test dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o dist/device-agent ./cmd/agent

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

clean:
	rm -rf dist

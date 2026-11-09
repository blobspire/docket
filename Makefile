VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build install test vet dist clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o docket ./cmd/docket

install:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(HOME)/.local/bin/docket ./cmd/docket

test:
	go test -race ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

# Static binaries for every supported platform, in dist/docket-<os>-<arch>.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "dist/docket-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/docket-$$os-$$arch ./cmd/docket || exit 1; \
	done

clean:
	rm -rf dist docket

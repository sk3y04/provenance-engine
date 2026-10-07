.PHONY: test vet lint fmt build engine-example release-dry-run clean all

# Version stamped into release artifact names. Falls back to "dev" outside a
# tagged checkout; the release workflow passes the pushed tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
DIST := dist

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -l .

build:
	go build -o provenance ./cmd/provenance/

# Compile the standalone external-consumer module that imports the public
# engine facade. It resolves the parent module through the repository go.work,
# so no replace directive is required (see docs/RELEASING.md).
engine-example:
	cd engine/externaltest && go build ./...

# Local approximation of the tag-triggered release build: cross-compile the
# `provenance` CLI for every published platform and write checksums. Mirrors
# .github/workflows/release.yml.
release-dry-run:
	rm -rf $(DIST)
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="$(DIST)/provenance_$(VERSION)_$${os}_$${arch}$$ext"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o "$$out" ./cmd/provenance || exit 1; \
	done
	@cd $(DIST) && (sha256sum * > checksums.txt 2>/dev/null || shasum -a 256 * > checksums.txt)
	@echo "release artifacts written to $(DIST)/"

clean:
	rm -f provenance
	rm -rf $(DIST)

all: vet lint test build engine-example
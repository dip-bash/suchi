# suchi
#
#   make build                    build for this machine  -> suchi
#   make cross-build VERSION=0.5.0   Linux x86-64 + ARM64 -> dist/
#   make test

VERSION ?= dev
BIN := suchi
BUILD_DIR := dist
CMD := ./cmd/suchi
LDFLAGS := -s -w -X github.com/dip-bash/suchi/internal/version.Version=$(VERSION)

.PHONY: build test cross-build clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

# Build Linux binaries for x86-64 and ARM64.  Version injected with VERSION=, e.g.
#   make cross-build VERSION=0.5.0
cross-build:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BIN)-linux-x86_64 $(CMD)
	GOOS=linux GOARCH=arm64  go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BIN)-linux-aarch64 $(CMD)
	@echo "built suchi $(VERSION):" && ls -lh $(BUILD_DIR)

test:
	go vet ./...
	go test ./...

clean:
	rm -rf $(BIN) $(BUILD_DIR)

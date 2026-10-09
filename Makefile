.PHONY: build test fuzz lint dev clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc ./cmd/tc

test:
	GOTOOLCHAIN=local go test -race ./...

fuzz:
	GOTOOLCHAIN=local go test ./internal/proto -run '^$$' -fuzz FuzzDecode -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/sanitize -run '^$$' -fuzz FuzzSanitize -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/e2ecrypto -run '^$$' -fuzz FuzzParseEnc -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/e2ecrypto -run '^$$' -fuzz FuzzDecrypt -fuzztime 30s

lint:
	GOTOOLCHAIN=local go vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed, skipping"

dev: build
	./bin/tc server --data ./dev-data --listen :7443

clean:
	rm -rf bin dev-data

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc-linux-amd64 ./cmd/tc

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc-linux-arm64 ./cmd/tc

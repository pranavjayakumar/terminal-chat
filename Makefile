.PHONY: build test test-malicious fuzz lint dev clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc ./cmd/tc

test: test-malicious
	GOTOOLCHAIN=local go test -race ./...

# Malicious-server suite (docs/E2E.md step 11): the server's test-only
# tamper hooks exist only with -tags tctamper.
test-malicious:
	GOTOOLCHAIN=local go test -race -tags tctamper ./internal/e2e -run Malicious

fuzz:
	GOTOOLCHAIN=local go test ./internal/proto -run '^$$' -fuzz FuzzDecode -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/proto -run '^$$' -fuzz FuzzDevices -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/sanitize -run '^$$' -fuzz FuzzSanitize -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/e2ecrypto -run '^$$' -fuzz FuzzParseEnc -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/e2ecrypto -run '^$$' -fuzz FuzzDecrypt -fuzztime 30s
	GOTOOLCHAIN=local go test ./internal/identity -run '^$$' -fuzz FuzzDeviceCert -fuzztime 30s

lint:
	GOTOOLCHAIN=local go vet ./...
	GOTOOLCHAIN=local go vet -tags tctamper ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed, skipping"

dev: build
	./bin/tc server --data ./dev-data --listen :7443

clean:
	rm -rf bin dev-data

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc-linux-amd64 ./cmd/tc

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc-linux-arm64 ./cmd/tc

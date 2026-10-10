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

# Every decoder of untrusted input (docs/E2E.md step 12): FUZZTIME=60s for a
# longer run. Crashers land in testdata/fuzz/ and then run as regression tests.
FUZZTIME ?= 30s
FUZZ := \
	proto:FuzzDecode proto:FuzzDevices proto:FuzzKeyOps \
	sanitize:FuzzSanitize \
	e2ecrypto:FuzzParseEnc e2ecrypto:FuzzDecrypt e2ecrypto:FuzzUnpad e2ecrypto:FuzzOpenBoxes \
	identity:FuzzDeviceCert identity:FuzzKeyEndorse identity:FuzzDeviceRevoke identity:FuzzEpochBundle \
	client:FuzzPlain client:FuzzKeyShare

fuzz:
	@set -e; for t in $(FUZZ); do \
		pkg=$${t%%:*}; name=$${t##*:}; \
		echo "== $$pkg $$name ($(FUZZTIME))"; \
		GOTOOLCHAIN=local go test ./internal/$$pkg -run '^$$' -fuzz "^$$name\$$" -fuzztime $(FUZZTIME); \
	done

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

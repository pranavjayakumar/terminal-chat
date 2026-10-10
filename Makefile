.PHONY: build test test-malicious fuzz lint dev clean release-sign

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

# Sign and publish the draft release the release workflow made for TAG
# (docs/DEPLOY.md "Cutting a release"): download its assets, check them
# against checksums.txt, sign checksums.txt with KEY (namespace tc-release),
# check the signature exactly as tc update will (embedded AllowedSigners),
# then upload checksums.txt.sig and publish.
release-sign:
	@printf '%s\n' "$(TAG)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "usage: make release-sign TAG=vX.Y.Z KEY=~/.ssh/id_ed25519" >&2; exit 1; }
	@test -n "$(KEY)" || { echo "usage: make release-sign TAG=vX.Y.Z KEY=~/.ssh/id_ed25519" >&2; exit 1; }
	@set -eu; key='$(KEY)'; case "$$key" in "~/"*) key="$$HOME/$${key#"~/"}" ;; esac; \
	d=$$(mktemp -d); trap 'rm -rf "$$d"' EXIT; \
	gh release download "$(TAG)" --dir "$$d" --pattern '*.tar.gz' --pattern checksums.txt; \
	(cd "$$d" && if command -v sha256sum >/dev/null; then sha256sum -c checksums.txt; else shasum -a 256 -c checksums.txt; fi); \
	echo "--- signing checksums.txt for $(TAG):"; cat "$$d/checksums.txt"; \
	ssh-keygen -Y sign -f "$$key" -n tc-release "$$d/checksums.txt"; \
	GOTOOLCHAIN=local go run ./internal/release/verifyrelease "$$d/checksums.txt" "$$d/checksums.txt.sig" || \
		{ echo "not published: embed this key in internal/release/signers.go and install.sh first" >&2; exit 1; }; \
	gh release upload "$(TAG)" "$$d/checksums.txt.sig" --clobber; \
	gh release edit "$(TAG)" --draft=false --latest; \
	echo "published $(TAG)"

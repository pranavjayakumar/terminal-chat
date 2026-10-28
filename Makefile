.PHONY: build build-voice release-voice test test-voice test-malicious fuzz lint dev clean release-sign

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tc ./cmd/tc

# Voice huddles need cgo + libopus (docs/VOICE.md): brew install opus /
# apt install libopus-dev. The default build above stays static and CGO-free.
build-voice:
	CGO_ENABLED=1 GOTOOLCHAIN=local go build -tags voice -trimpath -ldflags "$(LDFLAGS)" -o bin/tc-voice ./cmd/tc

# The release flavor: static libopus from a pinned tarball → dist/tc-voice_<os>_<arch>.tar.gz.
release-voice:
	sh scripts/build-voice.sh $$(go env GOOS) $$(go env GOARCH) $(VERSION) dist

test-voice:
	CGO_ENABLED=1 GOTOOLCHAIN=local go test -race -tags voice ./internal/voice/

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
	proto:FuzzDecode proto:FuzzDevices proto:FuzzKeyOps proto:FuzzParseLogin proto:FuzzParseDatagram voice:FuzzJitter \
	sanitize:FuzzSanitize notify:FuzzOSCSeq \
	e2ecrypto:FuzzParseEnc e2ecrypto:FuzzDecrypt e2ecrypto:FuzzUnpad e2ecrypto:FuzzOpenBoxes \
	identity:FuzzDeviceCert identity:FuzzKeyEndorse identity:FuzzDeviceRevoke identity:FuzzEpochBundle identity:FuzzInviteKey \
	client:FuzzPlain client:FuzzKeyShare client:FuzzGitHubKeys

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

# Sign and publish the draft release the release workflow made for TAG
# (docs/DEPLOY.md "Cutting a release"). Before signing, fail closed unless:
# checksums.txt carries exactly one "version TAG" line (rollback binding);
# every tarball matches checksums.txt; and every tarball has a GitHub build
# provenance attestation from release.yml run on refs/tags/TAG at the tag's
# commit (so a draft whose files someone swapped is never signed). Then sign
# checksums.txt with KEY (namespace tc-release), check it exactly as tc
# update will, upload checksums.txt.sig and publish.
REPO := pranavjayakumar/terminal-chat
release-sign:
	@printf '%s\n' "$(TAG)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "usage: make release-sign TAG=vX.Y.Z KEY=~/.ssh/id_ed25519" >&2; exit 1; }
	@test -n "$(KEY)" || { echo "usage: make release-sign TAG=vX.Y.Z KEY=~/.ssh/id_ed25519" >&2; exit 1; }
	@set -eu; key='$(KEY)'; case "$$key" in "~/"*) key="$$HOME/$${key#"~/"}" ;; esac; \
	commit=$$(gh api "repos/$(REPO)/commits/$(TAG)" --jq .sha); \
	printf '%s\n' "$$commit" | grep -Eq '^[0-9a-f]{40}$$' || { echo "can't resolve $(TAG) on GitHub" >&2; exit 1; }; \
	if local=$$(git rev-parse -q --verify "refs/tags/$(TAG)^{commit}"); then \
		test "$$local" = "$$commit" || { echo "$(TAG) is $$commit on GitHub but $$local here: tag moved? not signing" >&2; exit 1; }; \
	fi; \
	onmain=$$(gh api "repos/$(REPO)/compare/main...$$commit" --jq .status); \
	case "$$onmain" in behind | identical) ;; *) echo "$(TAG) ($$commit) is not on main ($$onmain): its release.yml may not be main's; not signing" >&2; exit 1 ;; esac; \
	echo "--- $(TAG) is commit $$commit (on main):";git log -1 --format='%H %an %ad%n    %s' "$$commit" 2>/dev/null || echo "    (not in this clone: git fetch --tags to inspect it)"; \
	d=$$(mktemp -d); trap 'rm -rf "$$d"' EXIT; \
	gh release download "$(TAG)" --repo "$(REPO)" --dir "$$d" --pattern '*.tar.gz' --pattern checksums.txt; \
	n=$$(awk '$$1 == "version" { n++ } END { print n + 0 }' "$$d/checksums.txt"); \
	test "$$n" = 1 && grep -qxF "version $(TAG)" "$$d/checksums.txt" || \
		{ echo "checksums.txt must carry exactly one 'version $(TAG)' line (release.yml writes it); not signing" >&2; exit 1; }; \
	awk '$$1 != "version"' "$$d/checksums.txt" > "$$d/sums.only"; \
	(cd "$$d" && if command -v sha256sum >/dev/null; then sha256sum -c sums.only; else shasum -a 256 -c sums.only; fi); \
	ls "$$d"/*.tar.gz >/dev/null; \
	for f in "$$d"/*.tar.gz; do \
		gh attestation verify "$$f" --repo "$(REPO)" \
			--signer-workflow "$(REPO)/.github/workflows/release.yml" \
			--source-ref "refs/tags/$(TAG)" --source-digest "$$commit" --signer-digest "$$commit" \
			--deny-self-hosted-runners >/dev/null || \
			{ echo "$$(basename "$$f"): no valid build provenance from release.yml at $(TAG) ($$commit); not signing" >&2; exit 1; }; \
		echo "provenance OK: $$(basename "$$f")"; \
	done; \
	echo "--- signing checksums.txt for $(TAG):"; cat "$$d/checksums.txt"; \
	ssh-keygen -Y sign -f "$$key" -n tc-release "$$d/checksums.txt"; \
	GOTOOLCHAIN=local go run ./internal/release/verifyrelease --tag "$(TAG)" "$$d/checksums.txt" "$$d/checksums.txt.sig" || \
		{ echo "not published: embed this key in internal/release/signers.go and install.sh first" >&2; exit 1; }; \
	gh release upload "$(TAG)" --repo "$(REPO)" "$$d/checksums.txt.sig" --clobber; \
	gh release edit "$(TAG)" --repo "$(REPO)" --draft=false --latest; \
	echo "published $(TAG)"; \
	scripts/brew-formula.sh "$(TAG)" "$$d" && \
	echo "Formula/tc.rb now points at $(TAG): commit it on a branch and open a PR (docs/DEPLOY.md, Homebrew)"

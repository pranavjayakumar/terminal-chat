package release_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The release workflow can't run here, so this pins its security
// properties (docs/DEPLOY.md "Cutting a release").
func TestReleaseWorkflowHardening(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	build, publish, ok := strings.Cut(wf, "\n  publish:\n")
	if !ok || !strings.Contains(build, "\n  build:\n") {
		t.Fatal("expected separate build and publish jobs")
	}
	if !strings.Contains(wf, "\npermissions: {}\n") {
		t.Error("workflow-level permissions must be empty")
	}
	// Every job before publish (build, voice, sums) is read-only.
	jobs := regexp.MustCompile(`(?m)^  [a-z-]+:$`).FindAllString(build[strings.Index(build, "\njobs:\n"):], -1)
	if len(jobs) < 3 || strings.Count(build, "      contents: read\n") != len(jobs) || strings.Contains(build, "write") {
		t.Errorf("jobs before publish %q must each be read-only", jobs)
	}
	if strings.Count(build, "persist-credentials: false") != strings.Count(build, "actions/checkout@") {
		t.Error("checkout must not persist the token")
	}
	if !strings.Contains(build, "tc-voice_linux_arm64") || !strings.Contains(build, `{ echo "version $TAG"; sha256sum -- *.tar.gz; } > checksums.txt`) {
		t.Error("checksums.txt must start with the version line and cover the voice builds")
	}
	if strings.Count(build, "cache: false") != strings.Count(build, "actions/setup-go@") {
		t.Error("release builds must not use the setup-go cache")
	}
	// Provenance: one attest job, the only one with id-token, before publish.
	_, attest, ok := strings.Cut(publish, "\n  attest:\n")
	if !ok || strings.Count(wf, "id-token: write") != 1 || !strings.Contains(attest, "id-token: write") ||
		!strings.Contains(attest, "attestations: write") || strings.Contains(attest, "contents:") ||
		!strings.Contains(attest, "actions/attest-build-provenance@") || !strings.Contains(attest, "subject-path: dist/*.tar.gz") {
		t.Error("attest job: id-token + attestations write only, attesting every tarball")
	}
	if !strings.Contains(publish, "needs: [build, voice, sums, attest]") {
		t.Error("the draft is created only after the tarballs are attested")
	}
	if strings.Contains(build, "GH_TOKEN") || strings.Contains(build, "github.token") {
		t.Error("build job must not see a token")
	}
	if !strings.Contains(publish, "contents: write") {
		t.Error("publish job: contents: write after build")
	}
	if !strings.Contains(publish, "--draft") || strings.Contains(publish, "--latest") {
		t.Error("the workflow must only create a draft (a maintainer signs and publishes)")
	}
	if !strings.Contains(build, `git merge-base --is-ancestor "$GITHUB_SHA" origin/main`) || !strings.Contains(build, "fetch-depth: 0") {
		t.Error("tagged commit must be checked against origin/main")
	}

	uses := regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)(.*)$`).FindAllStringSubmatch(wf, -1)
	if len(uses) == 0 {
		t.Fatal("no actions found")
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
	for _, u := range uses {
		if !pinned.MatchString(u[1]) || !strings.Contains(u[2], "# v") {
			t.Errorf("action %s%s is not pinned by commit SHA with a version comment", u[1], u[2])
		}
	}

	// make release-sign checks provenance and the version line before signing.
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	_, rs, _ := strings.Cut(string(mk), "\nrelease-sign:\n")
	sign := strings.Index(rs, "ssh-keygen -Y sign")
	for _, want := range []string{
		`grep -qxF "version $(TAG)"`,
		"gh attestation verify",
		`--signer-workflow "$(REPO)/.github/workflows/release.yml"`,
		`--source-ref "refs/tags/$(TAG)" --source-digest "$$commit"`,
		"--deny-self-hosted-runners",
		`compare/main...$$commit`,
	} {
		if i := strings.Index(rs, want); i < 0 || sign < 0 || i > sign {
			t.Errorf("make release-sign must run %s before signing", want)
		}
	}

	ci, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	ciJobs := regexp.MustCompile(`(?m)^  [a-z-]+:$`).FindAllString(string(ci[strings.Index(string(ci), "\njobs:\n"):]), -1)
	if strings.Count(string(ci), "\n    permissions:\n      contents: read\n") != len(ciJobs) || strings.Contains(string(ci), "write") {
		t.Errorf("ci jobs %q must each be read-only", ciJobs)
	}
	if strings.Count(string(ci), "persist-credentials: false") != strings.Count(string(ci), "actions/checkout@") {
		t.Error("ci checkout must not persist the token")
	}
	for _, u := range regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)(.*)$`).FindAllStringSubmatch(string(ci), -1) {
		if !pinned.MatchString(u[1]) || !strings.Contains(u[2], "# v") {
			t.Errorf("action %s%s is not pinned by commit SHA with a version comment", u[1], u[2])
		}
	}

	m := regexp.MustCompile(`\[\[ "\$TAG" =~ (\S+) \]\]`).FindStringSubmatch(build)
	if m == nil {
		t.Fatal("no tag check")
	}
	tagRE := regexp.MustCompile(m[1])
	for _, tag := range []string{"v0.2.0", "v10.20.30"} {
		if !tagRE.MatchString(tag) {
			t.Errorf("tag check rejects %q", tag)
		}
	}
	for _, tag := range []string{"v1.0.0-rc1", "v1.2.3'", "v1.2.3$x", "v1.2", "1.2.3", "v1.2.3.4", "v1.2.3\nx"} {
		if tagRE.MatchString(tag) {
			t.Errorf("tag check accepts %q", tag)
		}
	}
}

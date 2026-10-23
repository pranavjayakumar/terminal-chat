package release_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skailama/tc/internal/release"
	"github.com/skailama/tc/internal/release/releasetest"
)

var sums = []byte(strings.Repeat("ab", 32) + "  tc_linux_amd64.tar.gz\n")

func TestVerifySignature(t *testing.T) {
	key := releasetest.NewKey(t)
	signers := releasetest.AllowedSigners(key.PublicKey())
	sig := releasetest.Sign(t, key, release.SigNamespace, sums)
	if err := release.VerifySignature(signers, sums, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	other := releasetest.NewKey(t)
	tampered := append([]byte(nil), sums...)
	tampered[0] = 'c'
	flipped := bytes.Replace(sig, []byte("\n"), []byte("\nA"), 2) // shifts the base64 body
	for name, c := range map[string]struct {
		signers     string
		msg, sig    []byte
		wantNoKey   bool
		wantUnknown bool // well-formed, by a key not in the signers
	}{
		"tampered checksums":   {signers, tampered, sig, false, false},
		"other message's sig":  {signers, sums, releasetest.Sign(t, key, release.SigNamespace, tampered), false, false},
		"corrupt signature":    {signers, sums, flipped, false, false},
		"truncated signature":  {signers, sums, sig[:len(sig)/2], false, false},
		"not armored":          {signers, sums, []byte("hello"), false, false},
		"wrong namespace":      {signers, sums, releasetest.Sign(t, key, "git", sums), false, false},
		"unknown key":          {signers, sums, releasetest.Sign(t, other, release.SigNamespace, sums), false, true},
		"empty signers":        {"", sums, sig, false, false},
		"placeholder signers":  {release.Placeholder, sums, sig, true, false},
		"other principal":      {strings.Replace(signers, "tc-release ", "someone ", 1), sums, sig, false, false},
		"namespace not listed": {strings.Replace(signers, `namespaces="tc-release"`, `namespaces="git"`, 1), sums, sig, false, false},
		"cert-authority line":  {strings.Replace(signers, `namespaces="tc-release"`, `cert-authority,namespaces="tc-release"`, 1), sums, sig, false, false},
	} {
		err := release.VerifySignature(c.signers, c.msg, c.sig)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if c.wantNoKey != errors.Is(err, release.ErrNoSigningKey) {
			t.Errorf("%s: err %v", name, err)
		}
		// Only an untrusted key reads as "unknown key" (tc update then
		// points at the installer); tampering by a trusted key never does.
		if c.wantUnknown != errors.Is(err, release.ErrUnknownSigner) {
			t.Errorf("%s: ErrUnknownSigner mismatch: %v", name, err)
		}
	}

	// A line without options, a comment and a second key all work.
	multi := "# release keys\n" + releasetest.AllowedSigners(other.PublicKey()) + "\n" +
		strings.Replace(signers, `namespaces="tc-release" `, "", 1) + "\n"
	if err := release.VerifySignature(multi, sums, sig); err != nil {
		t.Fatalf("multi-key signers: %v", err)
	}
}

// The real tools: signatures from ssh-keygen -Y sign verify in Go, and the
// Go test signer (used for the install.sh tests) satisfies ssh-keygen -Y verify.
func TestSignatureInteropWithSSHKeygen(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	msg := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(msg, sums, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kt := range []string{"ed25519", "rsa", "ecdsa"} {
		k := filepath.Join(dir, "key_"+kt)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", kt, "-N", "", "-C", "maint", "-f", k).CombinedOutput(); err != nil {
			t.Fatalf("keygen %s: %v %s", kt, err, out)
		}
		os.Remove(msg + ".sig")
		if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-f", k, "-n", release.SigNamespace, msg).CombinedOutput(); err != nil {
			t.Fatalf("sign %s: %v %s", kt, err, out)
		}
		pub, _ := os.ReadFile(k + ".pub")
		sig, _ := os.ReadFile(msg + ".sig")
		signers := release.SigPrincipal + ` namespaces="` + release.SigNamespace + `" ` + string(pub)
		if err := release.VerifySignature(signers, sums, sig); err != nil {
			t.Fatalf("%s: ssh-keygen signature rejected: %v", kt, err)
		}
		if err := release.VerifySignature(signers, append(sums, '\n'), sig); err == nil {
			t.Fatalf("%s: tampered message accepted", kt)
		}
	}

	key := releasetest.NewKey(t)
	allowed := filepath.Join(dir, "allowed_signers")
	os.WriteFile(allowed, []byte(releasetest.AllowedSigners(key.PublicKey())+"\n"), 0o600)
	os.WriteFile(msg+".sig", releasetest.Sign(t, key, release.SigNamespace, sums), 0o600)
	verify := exec.Command("ssh-keygen", "-Y", "verify", "-f", allowed, "-I", release.SigPrincipal, "-n", release.SigNamespace, "-s", msg+".sig")
	verify.Stdin = bytes.NewReader(sums)
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen rejects the test signer: %v %s", err, out)
	}
}

func TestFetchRequiresSignature(t *testing.T) {
	ctx := context.Background()
	srv := releasetest.New(t, "v1.2.3")
	srv.Publish(t, "v1.2.3", map[string][]byte{"linux_amd64": []byte("GOOD")})
	if _, err := srv.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	// An attacker with release-write access swaps the tarball and checksums.txt.
	evil := releasetest.Tarball([]byte("EVIL"))
	srv.Put("v1.2.3", "tc_linux_amd64.tar.gz", evil)
	var b bytes.Buffer
	b.WriteString(sha256hex(evil) + "  tc_linux_amd64.tar.gz\n")
	srv.Put("v1.2.3", "checksums.txt", b.Bytes())
	if _, err := srv.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); !errors.Is(err, release.ErrSignature) {
		t.Fatalf("swapped checksums: err = %v", err)
	}
	// ...and signs it with their own key.
	srv.Put("v1.2.3", release.SigFile, releasetest.Sign(t, releasetest.NewKey(t), release.SigNamespace, b.Bytes()))
	if _, err := srv.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); !errors.Is(err, release.ErrSignature) {
		t.Fatalf("attacker-signed checksums: err = %v", err)
	}
	// ...or serves no signature at all.
	srv2 := releasetest.New(t, "v1.2.3")
	srv2.Put("v1.2.3", "tc_linux_amd64.tar.gz", evil)
	srv2.Put("v1.2.3", "checksums.txt", b.Bytes())
	if _, err := srv2.Source().Fetch(ctx, "v1.2.3", "linux", "amd64"); !errors.Is(err, release.ErrSignature) {
		t.Fatalf("unsigned release: err = %v", err)
	}
}

// A build whose embedded key is still the placeholder refuses updates
// before downloading anything.
func TestFetchPlaceholderKeyRefuses(t *testing.T) {
	srv := releasetest.New(t, "v1.2.3")
	srv.Publish(t, "v1.2.3", map[string][]byte{"linux_amd64": []byte("GOOD")})
	src := &release.Source{Base: srv.Base, Signers: release.Placeholder}
	if _, err := src.Fetch(context.Background(), "v1.2.3", "linux", "amd64"); !errors.Is(err, release.ErrNoSigningKey) {
		t.Fatalf("err = %v", err)
	}
	if len(srv.Hits()) != 0 {
		t.Fatalf("downloaded with no key: %v", srv.Hits())
	}
	// The embedded default: refused while it's the placeholder, else it parses.
	def := &release.Source{Base: srv.Base}
	_, err := def.Fetch(context.Background(), "v1.2.3", "linux", "amd64")
	if strings.Contains(release.AllowedSigners, release.Placeholder) {
		if !errors.Is(err, release.ErrNoSigningKey) {
			t.Fatalf("placeholder build: err = %v", err)
		}
	} else if !errors.Is(err, release.ErrSignature) {
		t.Fatalf("embedded key: err = %v (want the test release rejected as signed by another key)", err)
	}
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

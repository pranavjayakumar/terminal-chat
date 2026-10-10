package e2ecrypto

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// ReactTag is HMAC-SHA256(mk, "tc-react" || mid || emoji)[:12] in
// base64url after "~", bound to the key, scope, message and emoji.
func TestReactTag(t *testing.T) {
	f := newFixture(t)
	mid := MID([]byte("message"))
	tag, err := ReactTag(f.k, f.sid, f.ch, f.ep, mid, "👍")
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := hkdf.Key(sha256.New, f.k[:], nil, "tc-msg-v1"+string(scope(f.sid, f.ch, f.ep)), KeySize)
	rk, _ := hkdf.Key(sha256.New, mk, nil, "tc-react-v1", KeySize)
	m := hmac.New(sha256.New, rk)
	m.Write([]byte("tc-react"))
	m.Write(mid[:])
	m.Write([]byte("👍"))
	if want := "~" + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12]); tag != want {
		t.Fatalf("tag %q, want %q", tag, want)
	}
	if !ValidReactTag(tag) || len(tag) != ReactTagLen {
		t.Fatalf("tag %q not valid", tag)
	}
	again, _ := ReactTag(f.k, f.sid, f.ch, f.ep, mid, "👍")
	if again != tag {
		t.Fatal("tag not deterministic")
	}
	other := MID([]byte("other"))
	var k2 Key
	k2[0] = 1
	for name, fn := range map[string]func() (string, error){
		"emoji":   func() (string, error) { return ReactTag(f.k, f.sid, f.ch, f.ep, mid, "🎉") },
		"message": func() (string, error) { return ReactTag(f.k, f.sid, f.ch, f.ep, other, "👍") },
		"key":     func() (string, error) { return ReactTag(k2, f.sid, f.ch, f.ep, mid, "👍") },
		"channel": func() (string, error) { return ReactTag(f.k, f.sid, f.ch+1, f.ep, mid, "👍") },
		"epoch":   func() (string, error) { return ReactTag(f.k, f.sid, f.ch, f.ep+1, mid, "👍") },
	} {
		if got, _ := fn(); got == tag {
			t.Errorf("%s doesn't change the tag", name)
		}
	}
	for _, bad := range []string{"", "~", "👍", tag[1:], tag + "A", "!" + tag[1:], "~AAAAAAAAAAAAAAA=", "~AAAAAAAAAAAAAAA+"} {
		if ValidReactTag(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

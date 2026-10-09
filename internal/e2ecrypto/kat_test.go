package e2ecrypto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

const vectorsPath = "testdata/vectors.json"

type vectorFile struct {
	Comment string            `json:"comment"`
	Seed    string            `json:"seed"`
	Vectors map[string]string `json:"vectors"`
}

const katSeed = "tc-e2ecrypto-kat-v1"

// katPlains are the plaintexts encrypted in the vectors: empty, short, and both
// sides of the first pad-block boundary.
var katPlains = map[string][]byte{
	"empty": nil,
	"hello": []byte("hello, world"),
	"255":   bytes.Repeat([]byte{0xa5}, 255),
	"256":   bytes.Repeat([]byte{0x5a}, 256),
}

// generateVectors derives every output from a single deterministic stream, in a
// fixed order, so any change to a format, KDF label or field order changes bytes.
func generateVectors(t *testing.T) map[string]string {
	t.Helper()
	r := newDetRand(katSeed)
	v := map[string]string{}
	put := func(name string, b []byte) { v[name] = hex.EncodeToString(b) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	alice, err := Generate(r)
	must(err)
	bob, err := Generate(r)
	must(err)
	ab, err := alice.MarshalBinary()
	must(err)
	put("alice.device_keys", ab)
	put("alice.x25519_pub", alice.X25519Pub[:])
	put("alice.ed25519_pub", alice.Ed25519Pub)
	bb, err := bob.MarshalBinary()
	must(err)
	put("bob.device_keys", bb)
	put("bob.x25519_pub", bob.X25519Pub[:])

	var sid ServerID
	r.Read(sid[:])
	const ch, ep = uint64(0x0102030405060708), uint32(7)
	put("server_id", sid[:])

	k, err := NewEpochKey(r)
	must(err)
	kPrev, err := NewEpochKey(r)
	must(err)
	put("k", k[:])
	put("k_prev", kPrev[:])
	kc := KeyCommitment(k, sid, ch, ep)
	put("kc", kc[:])
	mk, err := msgKey(k, sid, ch, ep)
	must(err)
	put("mk", mk)

	eph, err := NewEphemeral(r)
	must(err)
	ephPub := [32]byte(eph.PublicKey().Bytes())
	put("eph_pub", ephPub[:])
	bk, err := boxKey(eph, bob.X25519Pub, ephPub, bob.X25519Pub, "tc-box-v1")
	must(err)
	put("box.bob.bk", bk)
	box, err := SealKey(eph, bob.X25519Pub, BoxAD{sid, ch, ep, bob.ID, alice.ID}, k)
	must(err)
	put("box.bob.ct", box[:])
	box, err = SealKey(eph, alice.X25519Pub, BoxAD{sid, ch, ep, alice.ID, alice.ID}, k)
	must(err)
	put("box.alice.ct", box[:])

	back, err := SealBack(r, k, kPrev, sid, ch, ep)
	must(err)
	put("back", back[:])

	bundle := []byte("bundle b bytes")
	put("bundle", bundle)
	eh := EH(bundle)
	put("eh", eh[:])
	chain := ChainHash(bundle)
	put("chain_hash", chain[:])
	sig, err := SignEd25519(alice.Ed25519, CtxEpoch, bundle)
	must(err)
	put("bundle_sig", sig)

	for _, name := range slices.Sorted(maps.Keys(katPlains)) {
		enc, err := Encrypt(r, k, alice.Ed25519, sid, ch, ep, eh, alice.ID, katPlains[name])
		must(err)
		put("enc."+name, enc)
		mid := MID(enc)
		put("mid."+name, mid[:])
	}
	put("pad.hello", pad256(katPlains["hello"]))
	return v
}

func TestKnownAnswers(t *testing.T) {
	got := generateVectors(t)
	if *update {
		out, err := json.MarshalIndent(vectorFile{
			Comment: "e2ecrypto known-answer vectors; regenerate with: go test ./internal/e2ecrypto -run TestKnownAnswers -update",
			Seed:    katSeed,
			Vectors: got,
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d vectors", len(got))
		return
	}
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if vf.Seed != katSeed {
		t.Fatalf("seed %q, want %q", vf.Seed, katSeed)
	}
	for _, name := range slices.Sorted(maps.Keys(vf.Vectors)) {
		if g, ok := got[name]; !ok {
			t.Errorf("%s: in vectors.json but no longer generated", name)
		} else if g != vf.Vectors[name] {
			t.Errorf("%s changed:\n got  %s\n want %s", name, g, vf.Vectors[name])
		}
	}
	for name := range got {
		if _, ok := vf.Vectors[name]; !ok {
			t.Errorf("%s: generated but missing from vectors.json (run -update)", name)
		}
	}
	checkStoredVectorsOpen(t, vf.Vectors)
}

// checkStoredVectorsOpen opens the checked-in bytes directly, so data written by
// an older build stays readable independently of how generation evolves.
func checkStoredVectorsOpen(t *testing.T, v map[string]string) {
	t.Helper()
	dec := func(name string) []byte {
		b, err := hex.DecodeString(v[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}
	var alice, bob DeviceKeys
	if err := alice.UnmarshalBinary(dec("alice.device_keys")); err != nil {
		t.Fatal(err)
	}
	if err := bob.UnmarshalBinary(dec("bob.device_keys")); err != nil {
		t.Fatal(err)
	}
	sid := ServerID(dec("server_id"))
	const ch, ep = uint64(0x0102030405060708), uint32(7)
	k := Key(dec("k"))

	got, err := OpenKey(bob.X25519, [32]byte(dec("eph_pub")), BoxAD{sid, ch, ep, bob.ID, alice.ID}, dec("box.bob.ct"))
	if err != nil || got != k {
		t.Fatalf("stored bob box: %v", err)
	}
	prev, err := OpenBack(k, sid, ch, ep, dec("back"))
	if err != nil || prev != Key(dec("k_prev")) {
		t.Fatalf("stored backlink: %v", err)
	}
	if KeyCommitment(k, sid, ch, ep) != [32]byte(dec("kc")) {
		t.Fatal("stored kc mismatch")
	}
	if !VerifyEd25519(alice.Ed25519Pub, CtxEpoch, dec("bundle"), dec("bundle_sig")) {
		t.Fatal("stored bundle sig")
	}
	for name, plain := range katPlains {
		enc := dec("enc." + name)
		p, err := Decrypt(k, alice.Ed25519Pub, sid, ch, enc)
		if err != nil || !bytes.Equal(p, plain) {
			t.Fatalf("stored enc.%s: %v", name, err)
		}
		h, err := ParseEnc(enc)
		if err != nil || h.Epoch != ep || h.EH != EpochHash(dec("eh")) || h.Dev != alice.ID {
			t.Fatalf("stored enc.%s header: %v", name, err)
		}
		if MID(enc) != MsgID(dec("mid."+name)) {
			t.Fatalf("stored mid.%s", name)
		}
	}
}

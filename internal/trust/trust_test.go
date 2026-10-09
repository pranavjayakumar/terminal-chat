package trust

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
)

var (
	sidA = e2ecrypto.ServerID{0xa}
	sidB = e2ecrypto.ServerID{0xb}
)

const bob = 2

func open(t *testing.T, home string) *Store {
	t.Helper()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "tc")
	return open(t, home), home
}

type key struct {
	fp   string
	wire []byte
}

func sshKey(t *testing.T) key {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key{ssh.FingerprintSHA256(pk), pk.Marshal()}
}

func head(epoch uint32, tag string) Head {
	h := Head{Epoch: epoch, Hash: sha256.Sum256([]byte(tag))}
	copy(h.EH[:], h.Hash[:])
	return h
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func fps(ks []SSHKey, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	var s []string
	for _, k := range ks {
		s = append(s, k.Fingerprint+"/"+string(k.How))
	}
	return strings.Join(s, ",")
}

// ---- files ----

func TestPermsFresh(t *testing.T) {
	s, home := newStore(t)
	if err := s.MarkServerE2E(sidA); err != nil { // force -wal/-shm into existence
		t.Fatal(err)
	}
	if m := mode(t, home); m != 0o700 {
		t.Errorf("home mode %v", m)
	}
	for _, f := range []string{FileName, FileName + "-wal", FileName + "-shm"} {
		if m := mode(t, filepath.Join(home, f)); m != 0o600 {
			t.Errorf("%s mode %v", f, m)
		}
	}
}

func TestPermsTightensExisting(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tc")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil { // defeat the umask
		t.Fatal(err)
	}
	for _, f := range []string{FileName, FileName + "-wal"} {
		p := filepath.Join(home, f)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := open(t, home)
	if err := s.MarkServerE2E(sidA); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, home); m != 0o700 {
		t.Errorf("home mode %v", m)
	}
	for _, f := range []string{FileName, FileName + "-wal", FileName + "-shm"} {
		if m := mode(t, filepath.Join(home, f)); m != 0o600 {
			t.Errorf("%s mode %v", f, m)
		}
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, home := newStore(t)
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(home); err == nil {
		s2.Close()
		t.Fatal("opened a newer schema")
	}
}

// ---- device keys ----

func TestDeviceKeysPersist(t *testing.T) {
	s, home := newStore(t)
	k1, err := s.DeviceKeys(sidA, bob)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := k1.MarshalBinary()
	if k1.ID == (e2ecrypto.DevID{}) {
		t.Fatal("zero device id")
	}
	again, err := s.DeviceKeys(sidA, bob)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := again.MarshalBinary(); !bytes.Equal(b, b1) {
		t.Fatal("second call returned different keys")
	}
	s.Close()

	s = open(t, home)
	re, err := s.DeviceKeys(sidA, bob)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := re.MarshalBinary(); !bytes.Equal(b, b1) {
		t.Fatal("keys changed across reopen")
	}
	if !bytes.Equal(re.Ed25519Pub, k1.Ed25519Pub) || re.X25519Pub != k1.X25519Pub {
		t.Fatal("public keys differ")
	}
	other, err := s.DeviceKeys(sidB, bob)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == k1.ID || other.X25519Pub == k1.X25519Pub {
		t.Fatal("servers share device keys")
	}
}

func TestDeviceKeysConcurrentFirstUse(t *testing.T) {
	s1, home := newStore(t)
	s2 := open(t, home)
	var wg sync.WaitGroup
	got := make([][]byte, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := s1
			if i%2 == 1 {
				s = s2
			}
			k, err := s.DeviceKeys(sidA, bob)
			if err != nil {
				t.Error(err)
				return
			}
			got[i], _ = k.MarshalBinary()
		}()
	}
	wg.Wait()
	for i := range got {
		if !bytes.Equal(got[i], got[0]) {
			t.Fatalf("caller %d got different keys", i)
		}
	}
}

// ---- SSH keys ----

func TestTOFUAndKeyChange(t *testing.T) {
	s, home := newStore(t)
	k1, k2 := sshKey(t), sshKey(t)

	if err := s.PinKey(sidA, bob, k1.fp, k1.wire, TOFU); err != nil {
		t.Fatal(err)
	}
	if err := s.PinKey(sidA, bob, k1.fp, k1.wire, TOFU); err != nil {
		t.Fatalf("re-pin same key: %v", err)
	}
	for range 2 { // repeated sightings record one pending change
		if err := s.PinKey(sidA, bob, k2.fp, k2.wire, TOFU); !errors.Is(err, ErrKeyChanged) {
			t.Fatalf("new key: %v", err)
		}
	}
	if got := fps(s.PinnedKeys(sidA, bob)); got != k1.fp+"/tofu" {
		t.Fatalf("pinned = %s", got)
	}
	pend, err := s.PendingKeyChanges(sidA)
	if err != nil || len(pend) != 1 || pend[0].User != bob || pend[0].Fingerprint != k2.fp ||
		!bytes.Equal(pend[0].Wire, k2.wire) || pend[0].FirstSeen.IsZero() {
		t.Fatalf("pending = %+v, %v", pend, err)
	}
	s.Close()

	s = open(t, home) // pending survives restarts
	if err := s.AcknowledgeKeyChange(sidA, bob, k2.fp); err != nil {
		t.Fatal(err)
	}
	if got := fps(s.PinnedKeys(sidA, bob)); got != k1.fp+"/tofu,"+k2.fp+"/tofu" {
		t.Fatalf("pinned after ack = %s", got)
	}
	if got := fps(s.PendingKeyChanges(sidA)); got != "" {
		t.Fatalf("pending after ack = %s", got)
	}
	if err := s.AcknowledgeKeyChange(sidA, bob, k2.fp); !errors.Is(err, ErrNoKeyChange) {
		t.Fatalf("second ack: %v", err)
	}
	if err := s.PinKey(sidA, bob, k2.fp, k2.wire, TOFU); err != nil {
		t.Fatalf("acknowledged key: %v", err)
	}
}

func TestPinUpgradesNeverDowngrades(t *testing.T) {
	s, _ := newStore(t)
	k := sshKey(t)
	for _, h := range []How{TOFU, Verified, TOFU, Endorsed} {
		if err := s.PinKey(sidA, bob, k.fp, k.wire, h); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
	if got := fps(s.PinnedKeys(sidA, bob)); got != k.fp+"/verified" {
		t.Fatalf("pinned = %s", got)
	}
}

func TestVerifiedNewKeyStillNeedsAck(t *testing.T) {
	s, _ := newStore(t)
	k1, k2 := sshKey(t), sshKey(t)
	if err := s.PinKey(sidA, bob, k1.fp, k1.wire, TOFU); err != nil {
		t.Fatal(err)
	}
	if err := s.PinKey(sidA, bob, k2.fp, k2.wire, TOFU); !errors.Is(err, ErrKeyChanged) {
		t.Fatal(err)
	}
	if err := s.PinKey(sidA, bob, k2.fp, k2.wire, Verified); !errors.Is(err, ErrKeyChanged) {
		t.Fatalf("verified new key: %v", err)
	}
	if err := s.AcknowledgeKeyChange(sidA, bob, k2.fp); err != nil {
		t.Fatal(err)
	}
	if got := fps(s.PinnedKeys(sidA, bob)); !strings.HasSuffix(got, k2.fp+"/verified") {
		t.Fatalf("pinned = %s", got)
	}
}

func TestEndorsedKey(t *testing.T) {
	s, _ := newStore(t)
	k1, k2, k3 := sshKey(t), sshKey(t), sshKey(t)
	if err := s.PinKey(sidA, bob, k2.fp, k2.wire, Endorsed); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("endorsed first key: %v", err)
	}
	if got := fps(s.PinnedKeys(sidA, bob)); got != "" {
		t.Fatalf("pinned = %s", got)
	}
	if err := s.PinKey(sidA, bob, k1.fp, k1.wire, TOFU); err != nil {
		t.Fatal(err)
	}
	// Seen before its endorsement arrived: the endorsement resolves the banner.
	if err := s.PinKey(sidA, bob, k2.fp, k2.wire, TOFU); !errors.Is(err, ErrKeyChanged) {
		t.Fatal(err)
	}
	if err := s.PinKey(sidA, bob, k2.fp, k2.wire, Endorsed); err != nil {
		t.Fatal(err)
	}
	if err := s.PinKey(sidA, bob, k3.fp, k3.wire, Endorsed); err != nil {
		t.Fatal(err)
	}
	if got := fps(s.PendingKeyChanges(sidA)); got != "" {
		t.Fatalf("pending = %s", got)
	}
	got := fps(s.PinnedKeys(sidA, bob))
	for _, want := range []string{k1.fp + "/tofu", k2.fp + "/endorsed", k3.fp + "/endorsed"} {
		if !strings.Contains(got, want) {
			t.Errorf("pinned %s lacks %s", got, want)
		}
	}
}

func TestPinKeyRejectsBadInput(t *testing.T) {
	s, _ := newStore(t)
	k1, k2 := sshKey(t), sshKey(t)
	for name, err := range map[string]error{
		"fp mismatch": s.PinKey(sidA, bob, k2.fp, k1.wire, TOFU),
		"bad wire":    s.PinKey(sidA, bob, k1.fp, []byte("junk"), TOFU),
		"bad how":     s.PinKey(sidA, bob, k1.fp, k1.wire, "trusted"),
		"empty how":   s.PinKey(sidA, bob, k1.fp, k1.wire, ""),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := fps(s.PinnedKeys(sidA, bob)); got != "" {
		t.Fatalf("pinned = %s", got)
	}
}

// ---- devices ----

func device(t *testing.T, user uint64) Device {
	t.Helper()
	k, err := e2ecrypto.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := Device{ID: k.ID, User: user, X25519: k.X25519Pub, Cert: []byte("cert")}
	copy(d.Ed25519[:], k.Ed25519Pub)
	return d
}

func TestDevices(t *testing.T) {
	s, home := newStore(t)
	d := device(t, bob)
	if err := s.PutDevice(sidA, d); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDevice(sidA, d); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	got, ok, err := s.Device(sidA, d.ID)
	if err != nil || !ok || !reflect.DeepEqual(got, d) {
		t.Fatalf("device = %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.Device(sidA, e2ecrypto.DevID{1}); ok || err != nil {
		t.Fatalf("unknown device: %v %v", ok, err)
	}

	for name, bad := range map[string]func(*Device){
		"other user":   func(x *Device) { x.User = 3 },
		"other x25519": func(x *Device) { x.X25519[0] ^= 1 },
		"other ed":     func(x *Device) { x.Ed25519[0] ^= 1 },
	} {
		x := d
		bad(&x)
		if err := s.PutDevice(sidA, x); !errors.Is(err, ErrDeviceConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.PutDevice(sidA, Device{ID: e2ecrypto.DevID{9}, User: bob}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no cert: %v", err)
	}

	if err := s.RevokeDevice(sidA, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDevice(sidA, d); err != nil { // Revoked=false must not un-revoke
		t.Fatal(err)
	}
	// Revocation seen before the cert.
	early := device(t, bob)
	if err := s.RevokeDevice(sidA, early.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDevice(sidA, early); err != nil {
		t.Fatal(err)
	}
	live := device(t, bob)
	if err := s.PutDevice(sidA, live); err != nil {
		t.Fatal(err)
	}
	viaPut := device(t, bob)
	viaPut.Revoked = true
	if err := s.PutDevice(sidA, viaPut); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = open(t, home)
	devs, err := s.Devices(sidA, bob)
	if err != nil || len(devs) != 4 {
		t.Fatalf("devices = %+v %v", devs, err)
	}
	for _, x := range devs {
		if want := x.ID != live.ID; x.Revoked != want {
			t.Errorf("device %x revoked = %v", x.ID, x.Revoked)
		}
	}
}

// ---- chain ----

func TestChainRollbackAndFork(t *testing.T) {
	s, home := newStore(t)
	const ch = 5
	if _, ok, err := s.ChainHead(sidA, ch); ok || err != nil {
		t.Fatalf("empty head: %v %v", ok, err)
	}
	steps := []struct {
		h    Head
		want error
	}{
		{head(1, "a1"), nil},
		{head(1, "a1"), nil}, // same head again
		{head(1, "b1"), ErrFork},
		{head(3, "a3"), nil}, // caller verified the intermediate bundle
		{head(2, "a2"), ErrRollback},
		{head(1, "a1"), ErrRollback},
		{head(3, "b3"), ErrFork},
		{head(0, "x"), ErrInvalid},
	}
	for i, st := range steps {
		if err := s.AdvanceChain(sidA, ch, st.h); !errors.Is(err, st.want) || (st.want == nil) != (err == nil) {
			t.Fatalf("step %d epoch %d: %v, want %v", i, st.h.Epoch, err, st.want)
		}
	}
	bad := head(4, "a4")
	bad.EH[0] ^= 1
	if err := s.AdvanceChain(sidA, ch, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("eh mismatch: %v", err)
	}
	s.Close()

	s = open(t, home)
	if h, ok, err := s.ChainHead(sidA, ch); err != nil || !ok || h != head(3, "a3") {
		t.Fatalf("head = %+v %v %v", h, ok, err)
	}
	if err := s.AdvanceChain(sidA, ch, head(2, "a2")); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback after reopen: %v", err)
	}
}

// Two processes (TUI + tc tail) racing to pin the next epoch: exactly one
// wins each epoch, the rest see ErrFork, and the file stays consistent.
func TestAdvanceChainConcurrentStores(t *testing.T) {
	s1, home := newStore(t)
	s2 := open(t, home)
	const (
		ch      = 9
		epochs  = 40
		writers = 4
	)
	wins := make([][writers]bool, epochs+1)
	var wg sync.WaitGroup
	start := make([]chan struct{}, epochs+1)
	for e := range start {
		start[e] = make(chan struct{})
	}
	results := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := []*Store{s1, s2}[w%2]
			for e := uint32(1); e <= epochs; e++ {
				<-start[e]
				err := s.AdvanceChain(sidA, ch, head(e, string(rune('a'+w))+string(rune(e))))
				if err == nil {
					wins[e][w] = true
				}
				results <- err
			}
		}()
	}
	for e := 1; e <= epochs; e++ {
		close(start[e])
		for range writers {
			if err := <-results; err != nil && !errors.Is(err, ErrFork) {
				t.Fatalf("epoch %d: %v", e, err)
			}
		}
	}
	wg.Wait()

	winner := -1
	for e := 1; e <= epochs; e++ {
		n := 0
		for w, won := range wins[e] {
			if won {
				n++
				winner = w
			}
		}
		if n != 1 {
			t.Fatalf("epoch %d: %d winners", e, n)
		}
	}
	for _, s := range []*Store{s1, s2} {
		h, ok, err := s.ChainHead(sidA, ch)
		if err != nil || !ok || h != head(epochs, string(rune('a'+winner))+string(rune(epochs))) {
			t.Fatalf("final head = %+v %v %v", h, ok, err)
		}
	}
	var res string
	if err := s1.db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		t.Fatalf("integrity_check = %q %v", res, err)
	}
}

const childEpochs = 60

// TestMain doubles as the child process for TestAdvanceChainTwoProcesses.
func TestMain(m *testing.M) {
	if spec := os.Getenv("TC_TRUST_CHILD"); spec != "" {
		os.Exit(childAdvance(spec))
	}
	os.Exit(m.Run())
}

// childAdvance tries every epoch in order and prints the ones it won.
func childAdvance(spec string) int {
	home, tag, _ := strings.Cut(spec, "|")
	s, err := Open(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer s.Close()
	for e := uint32(1); e <= childEpochs; e++ {
		err := s.AdvanceChain(sidA, 9, head(e, fmt.Sprint(tag, e)))
		switch {
		case err == nil:
			fmt.Println("win", e)
		case !errors.Is(err, ErrFork) && !errors.Is(err, ErrRollback):
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

// The same race across real processes, which is how the TUI and `tc tail`
// share the file. Each epoch is tried by both before either moves on to a
// higher one, so the first attempt at each epoch always wins.
func TestAdvanceChainTwoProcesses(t *testing.T) {
	s, home := newStore(t)
	tags := []string{"p", "q"}
	outs := make([]bytes.Buffer, len(tags))
	cmds := make([]*exec.Cmd, len(tags))
	for i, tag := range tags {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), "TC_TRUST_CHILD="+home+"|"+tag)
		cmd.Stdout, cmd.Stderr = &outs[i], os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	winner := map[uint32]string{}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %s: %v", tags[i], err)
		}
		for _, line := range strings.Fields(strings.ReplaceAll(outs[i].String(), "win ", "")) {
			var e uint32
			fmt.Sscan(line, &e)
			if prev, dup := winner[e]; dup {
				t.Fatalf("epoch %d won by %s and %s", e, prev, tags[i])
			}
			winner[e] = tags[i]
		}
	}
	if len(winner) != childEpochs {
		t.Fatalf("%d epochs won, want %d", len(winner), childEpochs)
	}
	h, ok, err := s.ChainHead(sidA, 9)
	if err != nil || !ok || h != head(childEpochs, fmt.Sprint(winner[childEpochs], childEpochs)) {
		t.Fatalf("final head = %+v %v %v", h, ok, err)
	}
	var res string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		t.Fatalf("integrity_check = %q %v", res, err)
	}
}

// ---- sticky flags ----

func TestStickyFlags(t *testing.T) {
	s, home := newStore(t)
	if ok, err := s.ServerUsesE2E(sidA); ok || err != nil {
		t.Fatalf("fresh server flag: %v %v", ok, err)
	}
	if err := s.MarkChannelE2E(sidA, 7); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.MarkServerE2E(sidA); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkChannelE2E(sidA, 7); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s = open(t, home)
	if ok, err := s.ServerUsesE2E(sidA); !ok || err != nil {
		t.Fatalf("server flag lost: %v %v", ok, err)
	}
	if ok, err := s.ChannelE2E(sidA, 7); !ok || err != nil {
		t.Fatalf("channel flag lost: %v %v", ok, err)
	}
	if ok, _ := s.ChannelE2E(sidA, 8); ok {
		t.Fatal("unmarked channel is e2e")
	}

	// The only exported ways to touch the flags are Mark* and the getters:
	// nothing the sync code can call clears them.
	allowed := map[string]bool{"MarkServerE2E": true, "ServerUsesE2E": true, "MarkChannelE2E": true, "ChannelE2E": true}
	typ := reflect.TypeOf(s)
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if strings.Contains(name, "E2E") && !allowed[name] {
			t.Errorf("unexpected flag method %s", name)
		}
		if strings.HasPrefix(name, "Clear") || strings.HasPrefix(name, "Unmark") || strings.HasPrefix(name, "Reset") {
			t.Errorf("unexpected method %s", name)
		}
	}
}

// ---- isolation ----

func TestPerServerIsolation(t *testing.T) {
	s, _ := newStore(t)
	kA, kB := sshKey(t), sshKey(t)
	if err := s.PinKey(sidA, bob, kA.fp, kA.wire, TOFU); err != nil {
		t.Fatal(err)
	}
	if err := s.PinKey(sidB, bob, kB.fp, kB.wire, TOFU); err != nil {
		t.Fatalf("other server's user is TOFU too: %v", err)
	}
	if got := fps(s.PinnedKeys(sidA, bob)); got != kA.fp+"/tofu" {
		t.Fatalf("A pinned = %s", got)
	}
	if err := s.PinKey(sidA, bob, kB.fp, kB.wire, TOFU); !errors.Is(err, ErrKeyChanged) {
		t.Fatal(err)
	}
	if got := fps(s.PendingKeyChanges(sidB)); got != "" {
		t.Fatalf("B pending = %s", got)
	}
	if err := s.AcknowledgeKeyChange(sidB, bob, kB.fp); !errors.Is(err, ErrNoKeyChange) {
		t.Fatalf("ack across servers: %v", err)
	}

	d := device(t, bob)
	if err := s.PutDevice(sidA, d); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Device(sidB, d.ID); ok {
		t.Fatal("device leaked to B")
	}
	if err := s.RevokeDevice(sidB, d.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Device(sidA, d.ID); got.Revoked {
		t.Fatal("B's revoke hit A")
	}
	other := d
	other.User = 3 // same dev id, different owner, on another server: no conflict
	if err := s.PutDevice(sidB, other); err != nil {
		t.Fatal(err)
	}

	if err := s.AdvanceChain(sidA, 5, head(4, "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceChain(sidB, 5, head(1, "b")); err != nil {
		t.Fatalf("B chain starts fresh: %v", err)
	}
	if h, _, _ := s.ChainHead(sidA, 5); h != head(4, "a") {
		t.Fatalf("A head = %+v", h)
	}

	if err := s.MarkChannelE2E(sidA, 5); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ServerUsesE2E(sidB); ok {
		t.Fatal("server flag leaked")
	}
	if ok, _ := s.ChannelE2E(sidB, 5); ok {
		t.Fatal("channel flag leaked")
	}
}

// Ids with the high bit set survive SQLite's signed integers.
func TestLargeIDs(t *testing.T) {
	s, _ := newStore(t)
	const big = ^uint64(0) - 1
	k := sshKey(t)
	if err := s.PinKey(sidA, big, k.fp, k.wire, TOFU); err != nil {
		t.Fatal(err)
	}
	ks, err := s.PinnedKeys(sidA, big)
	if err != nil || len(ks) != 1 || ks[0].User != big {
		t.Fatalf("keys = %+v %v", ks, err)
	}
	if err := s.AdvanceChain(sidA, big, head(^uint32(0), "max")); err != nil {
		t.Fatal(err)
	}
	if h, ok, _ := s.ChainHead(sidA, big); !ok || h.Epoch != ^uint32(0) {
		t.Fatalf("head = %+v", h)
	}
	if err := s.MarkChannelE2E(sidA, big); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ChannelE2E(sidA, big); !ok {
		t.Fatal("big channel flag")
	}
}

// ---- step 6 additions ----

func TestDeviceKeysBoundToUser(t *testing.T) {
	s, _ := newStore(t)
	k1, err := s.DeviceKeys(sidA, bob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeviceKeys(sidA, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("user 0: %v", err)
	}
	// TC_HOME re-used for another login on the same server: fresh keys.
	k3, err := s.DeviceKeys(sidA, 3)
	if err != nil || k3.ID == k1.ID {
		t.Fatalf("other user kept the device: %v", err)
	}
	// Retiring a device that is no longer current is a no-op.
	same, err := s.RetireDeviceKeys(sidA, 3, k1.ID)
	if err != nil || same.ID != k3.ID {
		t.Fatalf("retire of a stale id replaced keys: %v", err)
	}
	fresh, err := s.RetireDeviceKeys(sidA, 3, k3.ID)
	if err != nil || fresh.ID == k3.ID {
		t.Fatalf("retire: %v", err)
	}
	again, err := s.DeviceKeys(sidA, 3)
	if err != nil || again.ID != fresh.ID {
		t.Fatalf("retired keys not persisted: %v", err)
	}
}

func TestImportDeviceKeys(t *testing.T) {
	s, _ := newStore(t)
	k, _ := e2ecrypto.Generate(rand.Reader)
	if ok, err := s.ImportDeviceKeys(sidA, bob, k); !ok || err != nil {
		t.Fatalf("import: %v %v", ok, err)
	}
	other, _ := e2ecrypto.Generate(rand.Reader)
	if ok, err := s.ImportDeviceKeys(sidA, bob, other); ok || err != nil {
		t.Fatalf("second import overwrote: %v %v", ok, err)
	}
	got, err := s.DeviceKeys(sidA, bob)
	if err != nil || got.ID != k.ID {
		t.Fatalf("imported keys not used: %v", err)
	}
}

// A v1 file (step 3) gains the v2 columns and tables; its device keys are
// claimed by the first user that asks.
func TestMigrateV1(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tc")
	os.MkdirAll(home, 0o700)
	db, err := sql.Open("sqlite", filepath.Join(home, FileName))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := e2ecrypto.Generate(rand.Reader)
	raw, _ := k.MarshalBinary()
	for _, q := range []string{
		`CREATE TABLE device_keys (server_id BLOB PRIMARY KEY, keys BLOB NOT NULL, created INTEGER NOT NULL)`,
		`CREATE TABLE revoked_devices (server_id BLOB NOT NULL, dev BLOB NOT NULL, PRIMARY KEY (server_id, dev))`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO device_keys VALUES (?, ?, 1)`, sidA[:], raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO revoked_devices VALUES (?, ?)`, sidA[:], []byte{1}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s := open(t, home)
	got, err := s.DeviceKeys(sidA, bob)
	if err != nil || got.ID != k.ID {
		t.Fatalf("v1 keys lost: %v", err)
	}
	if err := s.AdvanceEpoch(sidA, 1, Epoch{Epoch: 1, Roster: []uint64{bob}}); err != nil {
		t.Fatalf("epochs table missing: %v", err)
	}
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d", v)
	}
}

func TestRevokeSeqKeepsEarliest(t *testing.T) {
	s, _ := newStore(t)
	d := device(t, bob)
	if err := s.PutDevice(sidA, d); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{50, 30, 70} {
		if err := s.RevokeDeviceAt(sidA, d.ID, seq); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := s.Device(sidA, d.ID)
	if !got.Revoked || got.RevokedSeq != 30 {
		t.Fatalf("revoked %v at %d, want 30", got.Revoked, got.RevokedSeq)
	}
	if err := s.RevokeDevice(sidA, d.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Device(sidA, d.ID); got.RevokedSeq != 0 {
		t.Fatalf("unknown-seq revoke didn't win: %d", got.RevokedSeq)
	}
}

func TestRevokeTimeKeepsEarliestKnown(t *testing.T) {
	s, _ := newStore(t)
	d := device(t, bob)
	if err := s.PutDevice(sidA, d); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{5000, 0, 3000, 9000} { // 0 = unknown, never wins
		if err := s.RevokeDeviceAtTime(sidA, d.ID, 40, at); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := s.Device(sidA, d.ID); !got.Revoked || got.RevokedSeq != 40 || got.RevokedTime != 3000 {
		t.Fatalf("revoked %v seq %d time %d, want 40 and 3000", got.Revoked, got.RevokedSeq, got.RevokedTime)
	}
}

// A v2 file (step 6 before revoke times) gains revoked_devices.time and
// keeps its revokes.
func TestMigrateV2(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tc")
	os.MkdirAll(home, 0o700)
	s0 := open(t, home)
	d := device(t, bob)
	if err := s0.PutDevice(sidA, d); err != nil {
		t.Fatal(err)
	}
	if err := s0.RevokeDeviceAt(sidA, d.ID, 7); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE revoked_devices DROP COLUMN time`, `PRAGMA user_version = 2`} {
		if _, err := s0.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s0.Close()

	s := open(t, home)
	if err := s.RevokeDeviceAtTime(sidA, d.ID, 7, 123); err != nil {
		t.Fatalf("time column missing: %v", err)
	}
	if got, _, _ := s.Device(sidA, d.ID); !got.Revoked || got.RevokedSeq != 7 || got.RevokedTime != 123 {
		t.Fatalf("after migration: %+v", got)
	}
}

func TestEpochsAndKeys(t *testing.T) {
	s, home := newStore(t)
	const ch = 4
	k1 := e2ecrypto.Key{1}
	e1 := Epoch{Epoch: 1, Hash: sha256.Sum256([]byte("e1")), KC: e2ecrypto.KeyCommitment(k1, sidA, ch, 1), Roster: []uint64{2, 5}}
	if err := s.AdvanceEpoch(sidA, ch, Epoch{Epoch: 2, Hash: e1.Hash, KC: e1.KC, Roster: e1.Roster}); !errors.Is(err, ErrGap) {
		t.Fatalf("epoch 2 without a head: %v", err)
	}
	bad := e1
	bad.Key = &e2ecrypto.Key{9}
	if err := s.AdvanceEpoch(sidA, ch, bad); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("bad key: %v", err)
	}
	if err := s.AdvanceEpoch(sidA, ch, e1); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceEpoch(sidA, ch, e1); err != nil {
		t.Fatalf("same head again: %v", err)
	}
	fork := e1
	fork.Hash[0] ^= 1
	if err := s.AdvanceEpoch(sidA, ch, fork); !errors.Is(err, ErrFork) {
		t.Fatalf("fork: %v", err)
	}
	if err := s.SetEpochKey(sidA, ch, 1, e2ecrypto.Key{2}); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("kc mismatch: %v", err)
	}
	if err := s.SetEpochKey(sidA, ch, 2, k1); !errors.Is(err, ErrUnknownEpoch) {
		t.Fatalf("unknown epoch: %v", err)
	}
	if m, _ := s.MissingKeys(sidA, ch); len(m) != 1 || m[0] != 1 {
		t.Fatalf("missing = %v", m)
	}
	if err := s.SetEpochKey(sidA, ch, 1, k1); err != nil {
		t.Fatal(err)
	}
	k2 := e2ecrypto.Key{3}
	e2 := Epoch{Epoch: 2, Hash: sha256.Sum256([]byte("e2")), KC: e2ecrypto.KeyCommitment(k2, sidA, ch, 2), Roster: []uint64{5}, Key: &k2}
	if err := s.AdvanceEpoch(sidA, ch, e2); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceEpoch(sidA, ch, e1); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback: %v", err)
	}
	s.Close()

	s = open(t, home)
	got, ok, err := s.Epoch(sidA, ch, 1)
	if err != nil || !ok || got.Key == nil || *got.Key != k1 || !reflect.DeepEqual(got.Roster, e1.Roster) || got.Hash != e1.Hash {
		t.Fatalf("epoch 1 = %+v %v %v", got, ok, err)
	}
	if h, _, _ := s.ChainHead(sidA, ch); h.Epoch != 2 || h.Hash != e2.Hash {
		t.Fatalf("head = %+v", h)
	}
	if m, _ := s.MissingKeys(sidA, ch); len(m) != 0 {
		t.Fatalf("missing = %v", m)
	}
	if _, ok, _ := s.Epoch(sidB, ch, 1); ok {
		t.Fatal("epoch leaked across servers")
	}
}

func TestAlarmsSticky(t *testing.T) {
	s, home := newStore(t)
	if _, ok, err := s.Alarm(sidA, 3); ok || err != nil {
		t.Fatalf("fresh alarm: %v %v", ok, err)
	}
	s.RaiseAlarm(sidA, 3, "fork at epoch 2")
	s.RaiseAlarm(sidA, 3, "later")
	s.Close()
	s = open(t, home)
	if d, ok, err := s.Alarm(sidA, 3); !ok || err != nil || d != "fork at epoch 2" {
		t.Fatalf("alarm = %q %v %v", d, ok, err)
	}
	if _, ok, _ := s.Alarm(sidB, 3); ok {
		t.Fatal("alarm leaked across servers")
	}
}

//go:build tctamper

package e2e

// Harness for the malicious-server suite (malicious_test.go, docs/E2E.md
// step 11): the real server built with -tags tctamper, whose hooks let a
// test act as an attacker holding the server and its database, and real
// clients over loopback.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

type frame = server.TamperFrame

// tamperer dispatches the server's hooks to whatever the test sets now.
// Hooks run under mu, so a hook may keep state without its own lock.
type tamperer struct {
	mu      sync.Mutex
	out     func(user uint64, f frame) []frame
	welcome func(user uint64, w *proto.Welcome)
}

func (tp *tamperer) setOut(fn func(user uint64, f frame) []frame) {
	tp.mu.Lock()
	tp.out = fn
	tp.mu.Unlock()
}

func (tp *tamperer) setWelcome(fn func(user uint64, w *proto.Welcome)) {
	tp.mu.Lock()
	tp.welcome = fn
	tp.mu.Unlock()
}

func (tp *tamperer) hooks() *server.Tamper {
	return &server.Tamper{
		Welcome: func(user uint64, w *proto.Welcome) {
			tp.mu.Lock()
			defer tp.mu.Unlock()
			if tp.welcome != nil {
				tp.welcome(user, w)
			}
		},
		Out: func(user uint64, f frame) []frame {
			tp.mu.Lock()
			defer tp.mu.Unlock()
			if tp.out == nil {
				return []frame{f}
			}
			return tp.out(user, f)
		},
	}
}

// malEnv is a tampering server and its users.
type malEnv struct {
	s    *srv
	addr string
	dir  string
	sid  e2ecrypto.ServerID
	us   []*user
	tp   *tamperer
}

// malTeam is team() on a tampering server: users[0] is an admin, and every
// device is verified by everyone before it returns.
func malTeam(t *testing.T, names ...string) *malEnv {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	if !server.TamperBuild {
		t.Fatal("built without -tags tctamper")
	}
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	env := &malEnv{s: s, addr: addr, dir: dir, tp: &tamperer{}}
	s.SetTamper(env.tp.hooks())
	for i, n := range names {
		env.us = append(env.us, login(t, s, addr, n, i == 0))
	}
	eventually(t, "online", func() bool {
		for _, u := range env.us {
			if !u.online() {
				return false
			}
		}
		return true
	})
	env.sid = env.us[0].sid(t)
	eventually(t, "devices verified", func() bool {
		for _, a := range env.us {
			for _, b := range env.us {
				if a.verifiedDevices(env.sid, b.Me().ID) != 1 {
					return false
				}
			}
		}
		return true
	})
	return env
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// encDM opens an encrypted DM between a and b and waits until both hold epoch 1.
func (env *malEnv) encDM(t *testing.T, a, b *user) uint64 {
	t.Helper()
	dm, err := a.OpenDM(ctxT(t), []uint64{b.Me().ID})
	if err != nil {
		t.Fatal(err)
	}
	if !dm.E2E {
		t.Fatal("DM not encrypted")
	}
	eventually(t, "DM epoch 1", func() bool { return a.e2eAt(dm.ID, 1)() && b.e2eAt(dm.ID, 1)() })
	return dm.ID
}

// encChan has creator make encrypted channel name and add members one by
// one (each add is a rotation); it waits until everyone is at the last epoch.
func (env *malEnv) encChan(t *testing.T, creator *user, name string, members ...*user) uint64 {
	t.Helper()
	ctx := ctxT(t)
	ch, err := creator.CreateEncryptedChannel(ctx, name, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "channel epoch 1", creator.e2eAt(ch.ID, 1))
	for _, m := range members {
		if err := creator.AddMember(ctx, ch.ID, m.Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	ep := uint32(1 + len(members))
	for _, u := range append([]*user{creator}, members...) {
		eventually(t, "everyone at the last epoch", u.e2eAt(ch.ID, ep))
	}
	return ch.ID
}

// rotateCap waits out the server's per-channel rotation cap (§13.5).
func rotateCap() { time.Sleep(2100 * time.Millisecond) }

// rotate has u start a new epoch of ch, waiting out the server's rotation
// caps (≤ 1 per channel per 2 s, ≤ 1 per channel and user per 10 s).
func rotate(t *testing.T, ctx context.Context, u *user, ch uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := u.RotateKey(ctx, ch)
		var pe *proto.Error
		if err == nil {
			return
		}
		if !errors.As(err, &pe) || pe.Code != proto.ErrRateLimited || time.Now().After(deadline) {
			t.Fatalf("rotate: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (env *malEnv) db() *sql.DB { return env.s.TamperDB() }

// inject appends an event to the server's log as if a handler had: it gets
// a real seq and fans out to whoever may see it.
func (env *malEnv) inject(t *testing.T, kind string, ch, actor uint64, data any) proto.Event {
	t.Helper()
	ctx := ctxT(t)
	evs, err := env.s.TamperWrite(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		ev, err := env.s.TamperAppendEvent(ctx, tx, kind, ch, actor, 0, data)
		return []proto.Event{ev}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs[0]
}

// injectMsg stores m as a new message (row and msg.new event), whatever the
// handlers would have said; it returns the message id.
func (env *malEnv) injectMsg(t *testing.T, m proto.Message) uint64 {
	t.Helper()
	ctx := ctxT(t)
	var mid []byte
	if m.Enc != nil {
		x := e2ecrypto.MID(m.Enc)
		mid = x[:]
	}
	m.Mid = mid
	var id uint64
	_, err := env.s.TamperWrite(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		ev, err := env.s.TamperAppendEvent(ctx, tx, proto.KMsgNew, m.Chan, m.Author, 0, m)
		if err != nil {
			return nil, err
		}
		m.ID, m.Time = ev.Seq, ev.Time
		ev.Data = proto.MustRaw(m)
		if _, err := tx.ExecContext(ctx, `UPDATE events SET data = ? WHERE seq = ?`, []byte(ev.Data), ev.Seq); err != nil {
			return nil, err
		}
		nonce := make([]byte, 16)
		rand.Read(nonce)
		var dup int
		tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chan = ? AND mid = ?`, m.Chan, mid).Scan(&dup)
		rowMid := mid
		if dup > 0 {
			rowMid = nil // the server's unique (chan, mid) index; the event still carries it
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, chan, author, body, thread, time, nonce, enc, mid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Chan, m.Author, m.Body, m.Thread, m.Time, nonce, m.Enc, rowMid); err != nil {
			return nil, err
		}
		id = m.ID
		return []proto.Event{ev}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// rawMsg is the server's stored copy of message id.
func (env *malEnv) rawMsg(t *testing.T, id uint64) proto.Message {
	t.Helper()
	m := proto.Message{ID: id}
	err := env.db().QueryRow(`SELECT chan, author, body, thread, enc FROM messages WHERE id = ?`, id).
		Scan(&m.Chan, &m.Author, &m.Body, &m.Thread, &m.Enc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// plaintextRows counts messages in ch the server holds in the clear.
func (env *malEnv) plaintextRows(t *testing.T, ch uint64) int {
	t.Helper()
	var n int
	if err := env.db().QueryRow(`SELECT COUNT(*) FROM messages WHERE chan = ? AND body != ''`, ch).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// bundles are ch's key.epoch bundles in the server's log, by seq.
func (env *malEnv) bundles(t *testing.T, ch uint64) []identity.EpochBundle {
	t.Helper()
	var out []identity.EpochBundle
	for _, s := range env.rawBundles(t, ch) {
		eb, err := identity.PeekEpochBundle(identity.Signed(s))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, eb)
	}
	return out
}

func (env *malEnv) rawBundles(t *testing.T, ch uint64) []proto.Signed {
	t.Helper()
	rows, err := env.db().Query(`SELECT data FROM events WHERE kind = ? AND chan = ? ORDER BY seq`, proto.KKeyEpoch, ch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []proto.Signed
	for rows.Next() {
		var raw []byte
		var d proto.KeyEpoch
		if err := rows.Scan(&raw); err != nil || proto.Unmarshal(raw, &d) != nil {
			t.Fatal("bad key.epoch row")
		}
		out = append(out, d.Bundle)
	}
	return out
}

// sealedTo reports whether any epoch box or key.share in the server's
// whole log is addressed to device dev: the only ways K ever leaves a
// member's device.
func (env *malEnv) sealedTo(t *testing.T, dev e2ecrypto.DevID) bool {
	t.Helper()
	rows, err := env.db().Query(`SELECT kind, data FROM events WHERE kind IN (?, ?)`, proto.KKeyEpoch, proto.KKeyShare)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var raw []byte
		if err := rows.Scan(&kind, &raw); err != nil {
			t.Fatal(err)
		}
		if kind == proto.KKeyShare {
			var d proto.KeyShareArgs
			if proto.Unmarshal(raw, &d) == nil && string(d.Dev) == string(dev[:]) {
				return true
			}
			continue
		}
		var d proto.KeyEpoch
		if proto.Unmarshal(raw, &d) != nil {
			continue
		}
		eb, err := identity.PeekEpochBundle(identity.Signed(d.Bundle))
		if err != nil {
			continue
		}
		for _, b := range eb.Boxes {
			if b.Dev == dev {
				return true
			}
		}
	}
	return false
}

// keysHeld lists the epochs of ch whose K u's trust store holds.
func (u *user) keysHeld(sid e2ecrypto.ServerID, ch uint64) []uint32 {
	var out []uint32
	for e := uint32(1); e <= 64; e++ {
		if u.epochKey(sid, ch, e) != nil {
			out = append(out, e)
		}
	}
	return out
}

func (u *user) dev(t *testing.T, sid e2ecrypto.ServerID) *e2ecrypto.DeviceKeys {
	t.Helper()
	k, err := u.Trust().DeviceKeys(sid, u.Me().ID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (u *user) head(t *testing.T, sid e2ecrypto.ServerID, ch uint64) trustEpoch {
	t.Helper()
	h, ok, err := u.Trust().ChainHead(sid, ch)
	if err != nil || !ok {
		t.Fatalf("no chain head for %d", ch)
	}
	ep, ok, err := u.Trust().Epoch(sid, ch, h.Epoch)
	if err != nil || !ok {
		t.Fatal("head epoch missing")
	}
	return trustEpoch{ep.Epoch, ep.Hash, ep.Roster, ep.Key}
}

type trustEpoch struct {
	epoch  uint32
	hash   [32]byte
	roster []uint64
	key    *e2ecrypto.Key
}

// recipients are the verified, live devices u's trust store has for users.
func (u *user) recipients(t *testing.T, sid e2ecrypto.ServerID, users ...uint64) []identity.Recipient {
	t.Helper()
	var out []identity.Recipient
	for _, id := range users {
		ds, err := u.Trust().Devices(sid, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if !d.Revoked {
				out = append(out, identity.Recipient{Dev: d.ID, X25519: d.X25519})
			}
		}
	}
	return out
}

// msgAny finds message id in ch, top level or a thread reply.
func (u *user) msgAny(ch, id uint64) (client.Message, bool) {
	for _, m := range u.Messages(ch, 500) {
		if m.ID == id {
			return m, true
		}
		for _, r := range u.Thread(m.ID) {
			if r.ID == id {
				return r, true
			}
		}
	}
	return client.Message{}, false
}

func (u *user) failed(ch uint64) string {
	for _, m := range u.Messages(ch, 200) {
		if m.Failed != "" {
			return m.Failed
		}
	}
	return ""
}

func asEvent(f frame) (proto.Event, bool) {
	var ev proto.Event
	if f.Type != proto.TEvent || proto.Unmarshal(f.Payload, &ev) != nil {
		return ev, false
	}
	return ev, true
}

func evFrame(ev proto.Event) frame {
	return frame{Type: proto.TEvent, Payload: proto.MustRaw(ev)}
}

// openWith runs a client for of's account (copied config) from a fresh
// home, signing in with the SSH key at keyPath: e.g. a key the attacker
// made the server accept for of.
func openWith(t *testing.T, of *user, keyPath string) *user {
	t.Helper()
	home := t.TempDir()
	cfg := of.Config()
	cfg.KeyPath = keyPath
	writeConfig(t, home, cfg)
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	u := &user{Client: c, cancel: cancel, home: home}
	t.Cleanup(func() { cancel(); c.Close() })
	return u
}

func writeConfig(t *testing.T, home string, cfg client.Config) {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rawSession is a bare protocol connection (cap e2e1) signed in with an SSH
// key: a malicious member sending what the real client never would.
type rawSession struct {
	t     *testing.T
	codec *proto.Codec
	conn  transport.Conn
	mu    sync.Mutex
	next  uint32
	wait  map[uint32]chan proto.Res
}

func dialRaw(t *testing.T, addr, pin, keyPath string) *rawSession {
	t.Helper()
	signer, err := identity.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.DialTCP(ctx, addr, pin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	codec := proto.NewCodec(conn)
	exp, _ := conn.Exporter()
	sig, err := identity.Sign(signer, exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(proto.THello, proto.Hello{Client: "raw", Caps: []string{proto.CapE2E},
		Auth: proto.Auth{Pubkey: signer.PublicKey().Marshal(), Sig: sig}}); err != nil {
		t.Fatal(err)
	}
	f, err := codec.Read()
	if err != nil || f.Type != proto.TWelcome {
		t.Fatalf("raw login: %v %#x", err, f.Type)
	}
	codec.SetMaxFrame(proto.MaxFrame)
	r := &rawSession{t: t, codec: codec, conn: conn, wait: map[uint32]chan proto.Res{}}
	go func() {
		for {
			f, err := codec.Read()
			if err != nil {
				return
			}
			if f.Type != proto.TRes {
				continue
			}
			var res proto.Res
			if f.Decode(&res) != nil {
				continue
			}
			r.mu.Lock()
			ch := r.wait[res.ID]
			delete(r.wait, res.ID)
			r.mu.Unlock()
			if ch != nil {
				ch <- res
			}
		}
	}()
	if res := r.call(proto.OpSync, proto.SyncArgs{}); !res.OK {
		t.Fatalf("raw sync: %v", res.Err)
	}
	return r
}

func (r *rawSession) call(op string, args any) proto.Res {
	r.t.Helper()
	r.mu.Lock()
	r.next++
	id := r.next
	ch := make(chan proto.Res, 1)
	r.wait[id] = ch
	r.mu.Unlock()
	if err := r.codec.Write(proto.TReq, proto.Req{ID: id, Op: op, Args: proto.MustRaw(args)}); err != nil {
		r.t.Fatal(err)
	}
	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		r.t.Fatalf("raw %s: no RES", op)
		return proto.Res{}
	}
}

// encFor seals plain (any bytes) as u's device in ch under u's verified
// head: a well-signed enc whose contents only u chose.
func (u *user) encFor(t *testing.T, sid e2ecrypto.ServerID, ch uint64, plain []byte) []byte {
	t.Helper()
	h := u.head(t, sid, ch)
	if h.key == nil {
		t.Fatal("no head key")
	}
	d := u.dev(t, sid)
	enc, err := e2ecrypto.Encrypt(rand.Reader, *h.key, d.Ed25519, sid, ch, h.epoch,
		e2ecrypto.EpochHash(h.hash[:e2ecrypto.IDSize]), d.ID, plain)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

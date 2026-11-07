package client

import (
	"bytes"
	"crypto/sha256"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// WaitReason says why an encrypted message is still SecWaiting, so the UI
// can say whether waiting helps (#94).
type WaitReason uint8

const (
	// WaitKey: no key for its epoch yet (or it can't be told yet).
	WaitKey WaitReason = iota
	// WaitDevice: the device that sealed it isn't known yet.
	WaitDevice
	// WaitKeyChange: it was sealed by a device of its author under an SSH
	// key that changed and isn't accepted: it opens after /accept-key.
	WaitKeyChange
	// WaitRoot: it's a reply whose thread root isn't cached yet.
	WaitRoot
	// WaitRestore: its channel is in a rollback or fork alarm (a server
	// restored from a backup looks like one) and it is sealed under an
	// epoch this device hasn't verified: above its pinned head, or another
	// bundle of a pinned epoch (a member who already accepted the restore
	// started a new key). Accepting the restore (/alarm-recover) judges it
	// again; until then it can't be told good or bad (#198).
	WaitRestore
)

func (w WaitReason) String() string {
	return [...]string{"key", "device", "key-change", "root", "restore"}[w]
}

// setWaitReasons fills Wait for ms's SecWaiting messages. It follows
// judgeEnc's order: epoch key, then device, then thread root. A message
// stored as invalid only because it names another bundle of its epoch
// shows as waiting (WaitRestore) while its channel's chain alarm lasts:
// RecoverChain judges exactly those again (as SecWaiting).
func (c *Client) setWaitReasons(ms []Message) {
	var w *waitJudge
	for i := range ms {
		m := &ms[i]
		if m.Sec != SecWaiting && m.Sec != SecInvalid || m.Deleted || m.ID == 0 {
			continue
		}
		if w == nil {
			w = c.newWaitJudge()
		}
		if m.Sec == SecInvalid && !w.chainAlarm(m.Chan) {
			continue
		}
		var enc []byte
		if c.db.QueryRow(`SELECT enc FROM msgs WHERE id = ?`, m.ID).Scan(&enc) != nil || enc == nil {
			continue
		}
		if m.Sec == SecInvalid {
			if w.otherBundle(m.Chan, enc) {
				m.Sec, m.Wait = SecWaiting, WaitRestore
			}
			continue
		}
		m.Wait = w.reason(m.Chan, m.Author, enc)
	}
}

type waitJudge struct {
	c       *Client
	sid     e2ecrypto.ServerID
	ok      bool
	memo    *keyMemo
	pending map[uint64][]string // user → fingerprints of pending key changes
	changed map[devKey]bool     // underKeyChange, once per (author, device)
	alarm   map[uint64]bool     // chainAlarm, once per channel
}

// chainAlarm reports whether ch has a stored rollback or fork alarm (the
// kind /alarm-recover may clear), not accepted yet.
func (w *waitJudge) chainAlarm(ch uint64) bool {
	if !w.ok {
		return false
	}
	if a, ok := w.alarm[ch]; ok {
		return a
	}
	if w.alarm == nil {
		w.alarm = map[uint64]bool{}
	}
	a := false
	if _, server, _ := w.c.trust.ServerAlarm(w.sid); !server {
		d, ok, _ := w.c.trust.Alarm(w.sid, ch)
		a = ok && chainAlarm(d)
	}
	w.alarm[ch] = a
	return a
}

// otherBundle: enc parses and names a bundle other than the one this device
// verified for its epoch, which is what openEnc calls invalid first.
// Anything else wrong with it (a bad signature, a forged or replayed copy
// of a verified one) stays invalid.
func (w *waitJudge) otherBundle(ch uint64, enc []byte) bool {
	e, err := e2ecrypto.ParseEnc(enc)
	if err != nil {
		return false
	}
	ep := w.memo.epoch(w.c.trust, w.sid, ch, e.Epoch)
	return ep != nil && !bytes.Equal(e.EH[:], ep.Hash[:e2ecrypto.IDSize])
}

type devKey struct {
	user uint64
	dev  e2ecrypto.DevID
}

func (c *Client) newWaitJudge() *waitJudge {
	w := &waitJudge{c: c, memo: newKeyMemo(), changed: map[devKey]bool{}}
	w.sid, w.ok = c.cacheServerID()
	return w
}

func (w *waitJudge) reason(ch, author uint64, enc []byte) WaitReason {
	if !w.ok {
		return WaitKey
	}
	e, err := e2ecrypto.ParseEnc(enc)
	if err != nil {
		return WaitKey
	}
	if ep := w.memo.epoch(w.c.trust, w.sid, ch, e.Epoch); ep == nil || ep.Key == nil {
		if w.chainAlarm(ch) {
			if head, ok, _ := w.c.trust.ChainHead(w.sid, ch); ok && e.Epoch > head.Epoch {
				return WaitRestore
			}
		}
		return WaitKey
	}
	if w.memo.device(w.c.trust, w.sid, e.Dev) == nil {
		k := devKey{author, e.Dev}
		changed, ok := w.changed[k]
		if !ok {
			changed = w.underKeyChange(author, e.Dev)
			w.changed[k] = changed
		}
		if changed {
			return WaitKeyChange
		}
		return WaitDevice
	}
	return WaitRoot
}

// underKeyChange reports whether a cached cert for dev, of author, verifies
// under one of author's pending (unaccepted) changed keys.
func (w *waitJudge) underKeyChange(author uint64, dev e2ecrypto.DevID) bool {
	if w.pending == nil {
		w.pending = map[uint64][]string{}
		ks, _ := w.c.trust.PendingKeyChanges(w.sid)
		for _, k := range ks {
			w.pending[k.User] = append(w.pending[k.User], k.Fingerprint)
		}
	}
	fps := w.pending[author]
	if len(fps) == 0 {
		return false
	}
	rows, err := w.c.db.Query(`SELECT cert FROM devices WHERE dev = ? AND user = ?`, dev[:], int64(author))
	if err != nil {
		return false
	}
	var certs [][]byte
	for rows.Next() {
		var raw []byte
		if rows.Scan(&raw) == nil {
			certs = append(certs, raw)
		}
	}
	rows.Close()
	for _, raw := range certs {
		h := sha256.Sum256(raw)
		var s proto.Signed
		if w.c.badCert(h) || proto.Unmarshal(raw, &s) != nil {
			continue
		}
		dc, err := identity.PeekDeviceCert(identity.Signed(s))
		if err != nil || dc.ServerID != w.sid || dc.User != author || dc.Dev != dev {
			continue
		}
		pk, err := ssh.ParsePublicKey(dc.SSH)
		if err != nil || !slices.Contains(fps, ssh.FingerprintSHA256(pk)) {
			continue
		}
		// Only a cert that really verifies under that key: the server can't
		// make an arbitrary message claim a key change.
		// A forged one is marked, so redraws don't check it again.
		if _, err := identity.Signed(s).VerifyDeviceCert(w.sid, dc.SSH); err != nil {
			w.c.markBadCert(h)
			continue
		}
		return true
	}
	return false
}

package client

import (
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
)

func (w WaitReason) String() string {
	return [...]string{"key", "device", "key-change", "root"}[w]
}

// setWaitReasons fills Wait for ms's SecWaiting messages. It follows
// judgeEnc's order: epoch key, then device, then thread root.
func (c *Client) setWaitReasons(ms []Message) {
	var w *waitJudge
	for i := range ms {
		m := &ms[i]
		if m.Sec != SecWaiting || m.Deleted || m.ID == 0 {
			continue
		}
		if w == nil {
			w = c.newWaitJudge()
		}
		var enc []byte
		if c.db.QueryRow(`SELECT enc FROM msgs WHERE id = ?`, m.ID).Scan(&enc) != nil || enc == nil {
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
}

func (c *Client) newWaitJudge() *waitJudge {
	w := &waitJudge{c: c, memo: newKeyMemo()}
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
		return WaitKey
	}
	if w.memo.device(w.c.trust, w.sid, e.Dev) == nil {
		if w.underKeyChange(author, e.Dev) {
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
		var s proto.Signed
		if proto.Unmarshal(raw, &s) != nil {
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
		if _, err := identity.Signed(s).VerifyDeviceCert(w.sid, dc.SSH); err == nil {
			return true
		}
	}
	return false
}

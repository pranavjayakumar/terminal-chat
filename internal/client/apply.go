package client

import (
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// syncPoint returns what to send in sync: a cache without a server_id has
// never been snapshotted, so it always asks for one.
func (c *Client) syncPoint() (since uint64, serverID []byte, err error) {
	if serverID, err = metaBytes(c.db, "server_id"); err != nil || len(serverID) == 0 {
		return 0, nil, err
	}
	since, err = metaUint(c.db, "last_seq")
	return since, serverID, err
}

// syncHook runs on the read goroutine when the sync RES arrives, i.e. after
// the snapshot/replay and before any buffered live event.
func (c *Client) syncHook(s *session) func(proto.Res) error {
	return func(r proto.Res) error {
		if !r.OK {
			return nil
		}
		var sr proto.SyncRes
		if len(r.Data) > 0 {
			if err := proto.Unmarshal(r.Data, &sr); err != nil {
				return err
			}
		}
		reset := sr.Reset && !s.sawSnapshot
		err := c.tx(func(tx *sql.Tx) error {
			if reset {
				if err := resetCache(tx, s.welcome.ServerID); err != nil {
					return err
				}
			}
			// An e2e1 reset snapshot carried every device. A session without
			// e2e1 skips device events while last_seq moves on, so the next
			// e2e1 session must fetch the full list again.
			switch {
			case !s.e2e():
				if _, err := tx.Exec(`DELETE FROM meta WHERE k = 'devices_full'`); err != nil {
					return err
				}
			case sr.Reset:
				if err := setMeta(tx, "devices_full", 1); err != nil {
					return err
				}
			}
			last, err := metaUint(tx, "last_seq")
			if err != nil {
				return err
			}
			if sr.Head > last {
				return setMeta(tx, "last_seq", sr.Head)
			}
			return nil
		})
		if err != nil {
			return err
		}
		s.syncing = false
		if reset {
			return c.afterReset()
		}
		return nil
	}
}

func (c *Client) afterReset() error {
	users, err := loadUsers(c.db)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.users = users
	if u, ok := users[c.me.ID]; ok {
		c.me = u
	}
	c.mu.Unlock()
	c.post(Update{Reset: true, Channels: true, Users: true})
	return nil
}

func (c *Client) tx(f func(*sql.Tx) error) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// applySnapshot applies one SNAPSHOT part. The first part during sync is
// the reset signal: reset snapshots precede the RES that announces them.
func (c *Client) applySnapshot(s *session, sn proto.Snapshot) error {
	reset := s.syncing && !s.sawSnapshot
	u := Update{Reset: reset, Channels: true, Users: reset || len(sn.Users) > 0}
	err := c.tx(func(tx *sql.Tx) error {
		if reset {
			if err := resetCache(tx, s.welcome.ServerID); err != nil {
				return err
			}
		}
		for _, x := range sn.Users {
			if err := upsertUser(tx, x); err != nil {
				return err
			}
		}
		for _, ch := range sn.Chans {
			if err := storeChan(tx, ch); err != nil {
				return err
			}
		}
		for _, g := range sn.Groups {
			if err := storeGroup(tx, g); err != nil {
				return err
			}
		}
		me := c.Me().ID
		for _, m := range sn.Msgs {
			if err := upsertMsg(tx, m, true); err != nil {
				return err
			}
			if m.Author == me {
				if err := matchOutbox(tx, m); err != nil {
					return err
				}
			}
			u.msgs(m.Chan)
		}
		for ch, seq := range sn.Read {
			if err := setRead(tx, ch, seq, false); err != nil {
				return err
			}
		}
		for _, d := range sn.Devs {
			if err := storeDevice(tx, d); err != nil {
				return err
			}
		}
		for _, k := range sn.Keys {
			if err := storeBundle(tx, 0, k.Bundle, k.Seq); err != nil {
				return err
			}
		}
		for i, r := range sn.Revoked {
			if err := storeRevoke(tx, r, seqAt(sn.RevokedSeqs, i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if reset {
		s.sawSnapshot = true
		if err := c.afterReset(); err != nil {
			return err
		}
	} else {
		c.memUsers(sn.Users)
	}
	c.post(u)
	return nil
}

// applyUsers stores users outside the event stream (WELCOME).
func (c *Client) applyUsers(us []proto.User) error {
	err := c.tx(func(tx *sql.Tx) error {
		for _, u := range us {
			if err := upsertUser(tx, u); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.memUsers(us)
	c.post(Update{Users: true})
	return nil
}

func (c *Client) memUsers(us []proto.User) {
	if len(us) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range us {
		c.users[u.ID] = u
		if u.ID == c.me.ID {
			c.me = u
		}
	}
}

// applyEvent applies one EVENT and advances last_seq in the same
// transaction; events at or below last_seq are ignored. Malformed data for a
// known kind is skipped (seq still advances) so one bad event can't wedge
// sync forever.
func (c *Client) applyEvent(ev proto.Event) (Update, error) {
	var u Update
	var newUser *proto.User
	me := c.Me().ID
	err := c.tx(func(tx *sql.Tx) error {
		last, err := metaUint(tx, "last_seq")
		if err != nil {
			return err
		}
		if ev.Seq <= last {
			return errSkip
		}
		if err := c.applyKind(tx, ev, me, &u, &newUser); err != nil {
			return err
		}
		return setMeta(tx, "last_seq", ev.Seq)
	})
	if err == errSkip {
		return c.appliedElsewhere(ev), nil
	}
	if err != nil {
		return Update{}, err
	}
	c.procSeq = max(c.procSeq, ev.Seq)
	if newUser != nil {
		c.memUsers([]proto.User{*newUser})
	}
	for _, m := range u.New {
		c.clearTyping(m.Chan, m.Author, &u)
	}
	return u, nil
}

// appliedElsewhere handles an event another process sharing this cache
// (e.g. `tc send` next to the TUI) already applied: the database is current,
// but this process still has to refresh its in-memory users and tell its UI.
// Events this process already handled itself produce nothing.
func (c *Client) appliedElsewhere(ev proto.Event) Update {
	if ev.Seq <= c.procSeq {
		return Update{}
	}
	c.procSeq = ev.Seq
	var u Update
	switch ev.Kind {
	case proto.KUserNew, proto.KUserUpdate:
		var usr proto.User
		if proto.Unmarshal(ev.Data, &usr) == nil {
			c.memUsers([]proto.User{usr})
		}
		u.Users = true
	case proto.KMsgNew:
		var m proto.Message
		if proto.Unmarshal(ev.Data, &m) == nil {
			u.New = append(u.New, m)
		}
	case proto.KGroupUpdate:
		u.Channels = true
	}
	if ev.Chan != 0 {
		u.msgs(ev.Chan)
		u.Channels = true
	}
	return u
}

type skipErr struct{}

func (skipErr) Error() string { return "skip" }

var errSkip error = skipErr{}

func (c *Client) applyKind(tx *sql.Tx, ev proto.Event, me uint64, u *Update, newUser **proto.User) error {
	decode := func(v any) bool { return len(ev.Data) > 0 && proto.Unmarshal(ev.Data, v) == nil }
	switch ev.Kind {
	case proto.KMsgNew:
		var m proto.Message
		if !decode(&m) || m.ID == 0 {
			return nil
		}
		if m.Chan == 0 {
			m.Chan = ev.Chan
		}
		if err := upsertMsg(tx, m, true); err != nil {
			return err
		}
		if m.Author == me {
			if err := matchOutbox(tx, m); err != nil {
				return err
			}
		}
		u.msgs(m.Chan)
		u.Channels = true
		u.New = append(u.New, m)
	case proto.KMsgEdit:
		var d proto.MsgEdit
		// An empty body marks an edit the server scrubbed because a later
		// edit or delete superseded it (PROTOCOL.md §6).
		if !decode(&d) || d.Body == "" {
			return nil
		}
		if _, err := tx.Exec(`UPDATE msgs SET body = ?, edited = ? WHERE id = ?`, d.Body, d.Edited, d.Msg); err != nil {
			return err
		}
		u.msgs(c.msgChan(tx, d.Msg, ev.Chan))
		u.Channels = true // a mention may appear or vanish
	case proto.KMsgDelete:
		var d proto.MsgRef
		if !decode(&d) {
			return nil
		}
		if _, err := tx.Exec(`UPDATE msgs SET body = '', deleted = 1 WHERE id = ?`, d.Msg); err != nil {
			return err
		}
		u.msgs(c.msgChan(tx, d.Msg, ev.Chan))
		u.Channels = true
	case proto.KReactAdd, proto.KReactRemove:
		var d proto.Reaction
		if !decode(&d) {
			return nil
		}
		ch, err := react(tx, d.Msg, d.Emoji, ev.Actor, ev.Kind == proto.KReactAdd)
		if err != nil {
			return err
		}
		if ch != 0 {
			u.msgs(ch)
		}
		return noteReaction(tx, d, ev, me)
	case proto.KPinAdd, proto.KPinRemove:
		var d proto.MsgRef
		if !decode(&d) || ev.Chan == 0 {
			return nil
		}
		var err error
		if ev.Kind == proto.KPinAdd {
			err = addPin(tx, ev.Chan, d.Msg)
		} else {
			err = removePin(tx, d.Msg)
		}
		if err != nil {
			return err
		}
		u.Channels = true
		u.msgs(ev.Chan)
	case proto.KChanNew, proto.KChanUpdate:
		var ch proto.Channel
		if !decode(&ch) || ch.ID == 0 {
			return nil
		}
		if err := storeChan(tx, ch); err != nil {
			return err
		}
		u.Channels = true
	case proto.KChanJoin:
		var d proto.ChanJoin
		if !decode(&d) {
			return nil
		}
		if d.Chan.ID != 0 {
			if !slices.Contains(d.Chan.Members, d.User) {
				d.Chan.Members = append(d.Chan.Members, d.User)
			}
			if err := storeChan(tx, d.Chan); err != nil {
				return err
			}
		} else if err := setMember(tx, ev.Chan, d.User, true); err != nil {
			return err
		}
		u.Channels = true
	case proto.KChanLeave:
		var d proto.UserRef
		if !decode(&d) || ev.Chan == 0 {
			return nil
		}
		if d.User == me {
			if err := c.forgetChan(tx, ev.Chan, me); err != nil {
				return err
			}
			u.msgs(ev.Chan)
		} else if err := setMember(tx, ev.Chan, d.User, false); err != nil {
			return err
		}
		u.Channels = true
	case proto.KUserNew, proto.KUserUpdate:
		var x proto.User
		if !decode(&x) || x.ID == 0 {
			return nil
		}
		if err := upsertUser(tx, x); err != nil {
			return err
		}
		*newUser = &x
		u.Users = true
		u.Channels = true // DM display names
	case proto.KGroupUpdate:
		var g proto.Group
		if !decode(&g) || !proto.ValidName(g.Name) {
			return nil
		}
		if err := storeGroup(tx, g); err != nil {
			return err
		}
		u.Channels = true // who a message mentions changed
	case proto.KDeviceNew:
		var d proto.Device
		if !decode(&d) {
			return nil
		}
		return storeDevice(tx, d)
	case proto.KDeviceRevoke:
		var d proto.DeviceRevokeArgs
		if !decode(&d) {
			return nil
		}
		return storeRevoke(tx, d.Revoke, ev.Seq)
	case proto.KKeyEpoch:
		var d proto.KeyEpoch
		if !decode(&d) || ev.Chan == 0 {
			return nil
		}
		return storeBundle(tx, ev.Chan, d.Bundle, ev.Seq)
	case proto.KKeyShare:
		var d proto.KeyShareArgs
		if !decode(&d) || ev.Chan == 0 {
			return nil
		}
		var dev *e2ecrypto.DevID
		if id, ok := c.ourDev(tx); ok {
			dev = &id
		}
		return storeShare(tx, ev.Chan, ev.Actor, ev.Seq, d, dev)
	case proto.KReadUpdate:
		var d proto.ReadUpdate
		if !decode(&d) {
			return nil
		}
		if err := setRead(tx, d.Chan, d.Seq, false); err != nil {
			return err
		}
		u.Channels = true
	}
	return nil
}

// msgChan finds a cached message's channel, falling back to the event's.
func (c *Client) msgChan(tx *sql.Tx, id, fallback uint64) uint64 {
	var ch uint64
	if tx.QueryRow(`SELECT chan FROM msgs WHERE id = ?`, id).Scan(&ch) == nil {
		return ch
	}
	return fallback
}

// forgetChan drops a channel we left: its messages and read marker go; a
// public channel stays listed (it is still visible and joinable) without us.
func (c *Client) forgetChan(tx *sql.Tx, id, me uint64) error {
	for _, q := range []string{`DELETE FROM reacts WHERE msg IN (SELECT id FROM msgs WHERE chan = ?)`, `DELETE FROM msgs WHERE chan = ?`, `DELETE FROM reads WHERE chan = ?`, `DELETE FROM drafts WHERE chan = ?`, `DELETE FROM epochs WHERE chan = ?`, `DELETE FROM key_shares WHERE chan = ?`, `DELETE FROM pins WHERE chan = ?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	ch, ok, err := getChan(tx, id)
	if err != nil || !ok {
		return err
	}
	if ch.Kind != proto.ChanPublic {
		_, err := tx.Exec(`DELETE FROM chans WHERE id = ?`, id)
		return err
	}
	return setMember(tx, id, me, false)
}

// ---- ephemeral ----

func (c *Client) ephemeral(e proto.Ephemeral) {
	switch e.Kind {
	case proto.EphTyping:
		if e.Chan == 0 || e.User == 0 || e.User == c.Me().ID {
			return
		}
		if cv, ok := c.Channel(e.Chan); !ok || !cv.Member {
			return
		}
		c.mu.Lock()
		m := c.typing[e.Chan]
		if m == nil {
			m = map[uint64]time.Time{}
			c.typing[e.Chan] = m
		}
		m[e.User] = time.Now()
		c.mu.Unlock()
		ch := e.Chan
		c.post(Update{Typing: map[uint64]bool{ch: true}})
		// Tell the UI when the indicator expires: one timer per channel,
		// pushed back on each event, however fast they arrive.
		c.mu.Lock()
		if t := c.typingTimers[ch]; t != nil {
			t.Reset(c.timing.type_ + 50*time.Millisecond)
		} else {
			c.typingTimers[ch] = time.AfterFunc(c.timing.type_+50*time.Millisecond, func() {
				c.mu.Lock()
				delete(c.typingTimers, ch)
				c.mu.Unlock()
				c.post(Update{Typing: map[uint64]bool{ch: true}})
			})
		}
		c.mu.Unlock()
	case proto.EphKeyNeed:
		select { // the worker answers; drop if it's swamped
		case c.keyNeeds <- e:
		default:
		}
	case proto.EphPresence:
		if e.User == 0 || c.User(e.User).ID == 0 {
			return
		}
		status := e.Status
		if len(status) > proto.MaxStatus {
			status = strings.ToValidUTF8(status[:proto.MaxStatus], "")
		}
		c.mu.Lock()
		c.presence[e.User] = Presence{State: e.State, Status: status}
		c.mu.Unlock()
		c.post(Update{Presence: true})
	}
}

func (c *Client) clearTyping(ch, user uint64, u *Update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.typing[ch][user]; ok {
		delete(c.typing[ch], user)
		if u.Typing == nil {
			u.Typing = map[uint64]bool{}
		}
		u.Typing[ch] = true
	}
}

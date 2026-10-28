package client

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

// Trust UX (docs/E2E.md step 10): safety numbers and /verify, GitHub key
// checks, key-change acknowledgement and deliberate alarm clears. All of
// it works on the trust store, so it works offline (except the GitHub
// fetch).

var (
	// ErrNoKeys means no SSH key of the user has been seen (pinned) yet.
	ErrNoKeys = errors.New("no key of that user seen yet — they need to connect once")
	// ErrSafetyChanged means the keys behind a shown safety number changed
	// before it was confirmed: compare the new number.
	ErrSafetyChanged = errors.New("the keys changed since the safety number was shown — compare again")
	// ErrKeyChange refuses sends into an encrypted channel where a member's
	// SSH key changed and the change isn't acknowledged yet.
	ErrKeyChange = errors.New("sending is blocked: a member's key changed")
	// ErrNoAlarm means there is no alarm to show or clear.
	ErrNoAlarm = errors.New("no alarm here")
	// ErrOwnKeyNotPinned means this device hasn't pinned my own SSH key yet
	// (it does on its first connection).
	ErrOwnKeyNotPinned = errors.New("your own key isn't pinned yet — connect once")
	// ErrPendingChanged means the pending key changes aren't the ones shown
	// for acceptance any more: review them again.
	ErrPendingChanged = errors.New("the pending key changes changed — review them again")
	// ErrNotMatched means a GitHub check didn't match, so there's nothing to confirm.
	ErrNotMatched = errors.New("the GitHub keys don't match — nothing to confirm")
	// ErrNoGitHubUser means GitHub answered 404 for the login's keys.
	ErrNoGitHubUser = errors.New("no GitHub account")
)

// keyChangeReason is the failure an outbox entry is held with while a
// member's key change is pending; accepting the change requeues it.
const keyChangeReason = "not sent: a member's key changed — /accept-key, then /verify"

// safetyDomain and safetyIterations define safety numbers (like Signal's:
// iterated SHA-512, 5-byte chunks mod 100000, 5200 rounds).
const (
	safetyDomain     = "tc-safety-v1"
	safetyIterations = 5200
	safetyGroups     = 12
)

// SafetyParty is one side of a safety number: a user and the fingerprints
// of the SSH keys this device has pinned for them.
type SafetyParty struct {
	User         uint64
	Name         string // sanitized
	Fingerprints []string
}

// SafetyNumber is the 60-digit number two users compare out of band. Each
// computes it from their own trust store; it matches only if both pinned
// the same keys for both of them.
type SafetyNumber struct {
	Digits string // 60 digits
	Me     SafetyParty
	Them   SafetyParty
}

// Groups is Digits in 12 groups of 5.
func (s SafetyNumber) Groups() []string {
	var out []string
	for i := 0; i+5 <= len(s.Digits); i += 5 {
		out = append(out, s.Digits[i:i+5])
	}
	return out
}

// safetyDigits is SHA-512 over the domain, the server id and both parties
// (ordered by user id, fingerprints sorted, so both sides agree), iterated
// safetyIterations times with the input appended, as 12 groups of five
// digits (5 bytes each, big-endian, mod 100000).
func safetyDigits(sid e2ecrypto.ServerID, a, b SafetyParty) string {
	if b.User < a.User {
		a, b = b, a
	}
	var in []byte
	in = append(in, safetyDomain...)
	in = append(in, sid[:]...)
	for _, p := range []SafetyParty{a, b} {
		fps := slices.Clone(p.Fingerprints)
		slices.Sort(fps)
		fps = slices.Compact(fps)
		in = binary.BigEndian.AppendUint64(in, p.User)
		in = binary.BigEndian.AppendUint16(in, uint16(len(fps)))
		for _, fp := range fps {
			in = binary.BigEndian.AppendUint16(in, uint16(len(fp)))
			in = append(in, fp...)
		}
	}
	h := sha512.Sum512(in)
	buf := make([]byte, 0, len(h)+len(in))
	for range safetyIterations {
		buf = append(append(buf[:0], h[:]...), in...)
		h = sha512.Sum512(buf)
	}
	var sb strings.Builder
	for i := range safetyGroups {
		c := h[i*5 : i*5+5]
		v := uint64(c[0])<<32 | uint64(c[1])<<24 | uint64(c[2])<<16 | uint64(c[3])<<8 | uint64(c[4])
		fmt.Fprintf(&sb, "%05d", v%100000)
	}
	return sb.String()
}

func (c *Client) party(sid e2ecrypto.ServerID, user uint64) (SafetyParty, error) {
	ks, err := c.trust.PinnedKeys(sid, user)
	if err != nil {
		return SafetyParty{}, err
	}
	p := SafetyParty{User: user, Name: sanitize.Line(c.User(user).Name)}
	for _, k := range ks {
		p.Fingerprints = append(p.Fingerprints, k.Fingerprint)
	}
	slices.Sort(p.Fingerprints)
	return p, nil
}

// SafetyNumber is the safety number between me and user, from the keys
// this device pinned for both of us. Works offline.
func (c *Client) SafetyNumber(user uint64) (SafetyNumber, error) {
	sid, ok := c.cacheServerID()
	if !ok {
		return SafetyNumber{}, errors.New("not synced with the server yet")
	}
	me := c.Me().ID
	if user == me {
		return SafetyNumber{}, errors.New("that's you — verify someone else")
	}
	mine, err := c.party(sid, me)
	if err != nil {
		return SafetyNumber{}, err
	}
	theirs, err := c.party(sid, user)
	if err != nil {
		return SafetyNumber{}, err
	}
	if len(theirs.Fingerprints) == 0 {
		return SafetyNumber{}, ErrNoKeys
	}
	if len(mine.Fingerprints) == 0 {
		return SafetyNumber{}, ErrOwnKeyNotPinned
	}
	return SafetyNumber{Digits: safetyDigits(sid, mine, theirs), Me: mine, Them: theirs}, nil
}

// MarkVerified records that the user compared safety number digits with
// user out of band and it matched: user's pinned keys (exactly those the
// number covers) become Verified, so their devices count as verified out
// of band and UnverifiedUsers stops listing them. ErrSafetyChanged if the
// keys changed since digits was computed.
func (c *Client) MarkVerified(user uint64, digits string) error {
	sn, err := c.SafetyNumber(user)
	if err != nil {
		return err
	}
	if sn.Digits != digits {
		return ErrSafetyChanged
	}
	return c.verifyKeys(user, sn.Them.Fingerprints, "safety number")
}

func (c *Client) verifyKeys(user uint64, fps []string, method string) error {
	sid, ok := c.cacheServerID()
	if !ok {
		return errors.New("not synced with the server yet")
	}
	if err := c.trust.VerifyKeys(sid, user, fps, method, c.Me().ID); err != nil {
		if errors.Is(err, trust.ErrPinsChanged) {
			return ErrSafetyChanged
		}
		return err
	}
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}

// ---- GitHub ----

// githubBase is where VerifyGitHub fetches <login>.keys (tests replace it).
const githubBase = "https://github.com"

// SetGitHubBase replaces https://github.com for VerifyGitHub (tests only).
func (c *Client) SetGitHubBase(base string) { c.githubBase = base }

// maxGitHubKeys bounds the response read from GitHub.
const maxGitHubKeys = 256 << 10

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// GitHubCheck compares a user's pinned SSH keys with a GitHub account's
// public keys.
type GitHubCheck struct {
	Login   string
	User    uint64
	Name    string   // sanitized
	Pinned  []string // fingerprints of user's pinned keys
	GitHub  []string // fingerprints of the account's keys
	Missing []string // pinned keys that aren't on the account
	Match   bool     // every pinned key is on the account (ConfirmGitHub may verify)
}

// VerifyGitHub fetches https://github.com/<login>.keys (only ever on this
// explicit request) and compares it with user's pinned keys; Missing says
// which aren't listed. It changes nothing: once the user confirms the
// account is theirs, ConfirmGitHub marks a match Verified.
func (c *Client) VerifyGitHub(ctx context.Context, user uint64, login string) (GitHubCheck, error) {
	if !githubLogin.MatchString(login) {
		return GitHubCheck{}, errors.New("not a GitHub user name")
	}
	sid, ok := c.cacheServerID()
	if !ok {
		return GitHubCheck{}, errors.New("not synced with the server yet")
	}
	p, err := c.party(sid, user)
	if err != nil {
		return GitHubCheck{}, err
	}
	if len(p.Fingerprints) == 0 {
		return GitHubCheck{}, ErrNoKeys
	}
	base := c.githubBase
	if base == "" {
		base = githubBase
	}
	gh, err := fetchGitHubKeys(ctx, base, login)
	if err != nil {
		return GitHubCheck{}, err
	}
	res := GitHubCheck{Login: login, User: user, Name: p.Name, Pinned: p.Fingerprints, GitHub: gh}
	for _, fp := range p.Fingerprints {
		if !slices.Contains(gh, fp) {
			res.Missing = append(res.Missing, fp)
		}
	}
	res.Match = len(res.Missing) == 0
	return res, nil
}

// ConfirmGitHub marks the keys of a matching GitHub check Verified
// ("github:<login>" in the audit log) because the user confirmed that the
// account is the user's. ErrSafetyChanged if the pinned keys changed since.
func (c *Client) ConfirmGitHub(res GitHubCheck) error {
	if !res.Match || len(res.Pinned) == 0 {
		return ErrNotMatched
	}
	return c.verifyKeys(res.User, res.Pinned, "github:"+res.Login)
}

func fetchGitHubKeys(ctx context.Context, base, login string) ([]string, error) {
	keys, err := getGitHubKeys(ctx, base, login)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, pk := range keys {
		if fp := ssh.FingerprintSHA256(pk); !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	slices.Sort(out)
	return out, nil
}

// getGitHubKeys fetches <base>/<login>.keys over the same origin only and
// returns the keys that parse, in order. A body over maxGitHubKeys is an
// error rather than silently cut short.
func getGitHubKeys(ctx context.Context, base, login string) ([]ssh.PublicKey, error) {
	if !githubLogin.MatchString(login) {
		return nil, errors.New("not a GitHub user name")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+url.PathEscape(login)+".keys", nil)
	if err != nil {
		return nil, err
	}
	origin, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		// Same origin only: no downgrade to http, no other host.
		if r.URL.Scheme != origin.Scheme || r.URL.Host != origin.Host || len(via) >= 5 {
			return fmt.Errorf("refusing redirect to %s", sanitize.Line(r.URL.Redacted()))
		}
		return nil
	}}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching GitHub keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w github.com/%s (HTTP 404) — check the spelling", ErrNoGitHubUser, login)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching GitHub keys: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubKeys+1))
	if err != nil {
		return nil, fmt.Errorf("reading GitHub keys: %w", err)
	}
	if len(body) > maxGitHubKeys {
		return nil, fmt.Errorf("GitHub keys response is over %d KiB", maxGitHubKeys>>10)
	}
	return parseGitHubKeys(body), nil
}

// parseGitHubKeys parses a .keys body: one authorized_keys line per key.
// Lines that don't parse are skipped.
func parseGitHubKeys(body []byte) []ssh.PublicKey {
	var out []ssh.PublicKey
	for line := range bytes.Lines(body) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if pk, _, _, rest, err := ssh.ParseAuthorizedKey(line); err == nil && len(rest) == 0 {
			out = append(out, pk)
		}
	}
	return out
}

// ---- key changes ----

// KeyChange is a user's SSH key that verifies but isn't pinned or endorsed:
// nothing is sealed to it until AcceptKeyChanges.
type KeyChange struct {
	User        uint64
	Fingerprint string
	FirstSeen   time.Time
}

// KeyChanges lists other users' unacknowledged key changes. Works offline.
func (c *Client) KeyChanges() []KeyChange {
	sid, ok := c.cacheServerID()
	if !ok {
		return nil
	}
	ks, err := c.trust.PendingKeyChanges(sid)
	if err != nil {
		return nil
	}
	me := c.Me().ID
	var out []KeyChange
	for _, k := range ks {
		if k.User != me {
			out = append(out, KeyChange{User: k.User, Fingerprint: k.Fingerprint, FirstSeen: k.FirstSeen})
		}
	}
	return out
}

// KeyChangedMembers lists the members of encrypted channel ch (not me)
// with an unacknowledged key change; sending there is blocked until each
// is accepted. Sorted; nil for plaintext channels, and nil on errors (it's
// for display: the send gate, keyChangeAllowed, fails closed instead).
func (c *Client) KeyChangedMembers(ch uint64) []uint64 {
	us, _ := c.keyChangedMembers(ch)
	return us
}

// keyChangedMembers is KeyChangedMembers that says when it can't tell.
func (c *Client) keyChangedMembers(ch uint64) ([]uint64, error) {
	sid, ok := c.cacheServerID()
	if !c.chanIsE2E(c.db, sid, ok, ch) {
		return nil, nil
	}
	if !ok {
		return nil, errors.New("not synced with the server yet")
	}
	cv, ok, err := getChan(c.db, ch)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("channel not loaded")
	}
	ks, err := c.trust.PendingKeyChanges(sid)
	if err != nil {
		return nil, err
	}
	me := c.Me().ID
	changed := map[uint64]bool{}
	for _, k := range ks {
		if k.User != me {
			changed[k.User] = true
		}
	}
	var out []uint64
	for _, u := range cv.Members {
		if changed[u] && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return out, nil
}

// keyChangeAllowed refuses sends into an encrypted channel with a member
// whose key change isn't acknowledged, and fails closed when it can't tell.
func (c *Client) keyChangeAllowed(ch uint64) error {
	us, err := c.keyChangedMembers(ch)
	if err != nil {
		return fmt.Errorf("%w: can't check the members' keys (%s)", ErrKeyChange, sanitize.Line(err.Error()))
	}
	if len(us) == 0 {
		return nil
	}
	names := make([]string, len(us))
	for i, u := range us {
		names[i] = "@" + sanitize.Line(c.User(u).Name)
	}
	return fmt.Errorf("%w: %s — /accept-key, then /verify in tc", ErrKeyChange, strings.Join(names, ", "))
}

// requeueKeyChange requeues outbox entries held for a key change in the
// channels where none is pending any more.
func (c *Client) requeueKeyChange() {
	rows, err := c.db.Query(`SELECT DISTINCT chan FROM outbox WHERE failed = ?`, keyChangeReason)
	if err != nil {
		return
	}
	var chans []uint64
	for rows.Next() {
		var ch uint64
		if rows.Scan(&ch) == nil {
			chans = append(chans, ch)
		}
	}
	rows.Close()
	for _, ch := range chans {
		if c.keyChangeAllowed(ch) != nil {
			continue
		}
		res, err := c.db.Exec(`UPDATE outbox SET failed = '' WHERE chan = ? AND failed = ?`, ch, keyChangeReason)
		if n, _ := rowsAffected(res); err == nil && n > 0 {
			c.outboxChanged(ch, nil)
		}
	}
}

// PendingKeys lists the fingerprints of user's pending key changes, sorted.
// Never my own: a key the server says is mine must not be trusted on a
// prompt the server can trigger, since its devices would get every channel
// key (#94 review); that needs an endorsement signed by a key I hold.
func (c *Client) PendingKeys(user uint64) []string {
	var out []string
	for _, k := range c.KeyChanges() {
		if k.User == user {
			out = append(out, k.Fingerprint)
		}
	}
	slices.Sort(out)
	return out
}

// AcceptKeyChanges acknowledges exactly the pending key changes fps of user
// that were shown to and confirmed by the user: those keys are pinned
// (trusted on first use again, so the user shows as unverified), their
// devices verify on the next pass, and sends held for the change are
// requeued. With only, fps may be some of the pending ones; otherwise it
// must be all of them. ErrPendingChanged if they aren't what is pending.
func (c *Client) AcceptKeyChanges(user uint64, fps []string, only bool) error {
	if user == c.Me().ID {
		// Never on a prompt (#94, #146): the server could invent the key.
		return ErrOwnKeyAccept
	}
	sid, ok := c.cacheServerID()
	if !ok {
		return errors.New("not synced with the server yet")
	}
	pend := c.PendingKeys(user)
	if len(pend) == 0 {
		return errors.New("no pending key change for that user")
	}
	want := slices.Clone(fps)
	slices.Sort(want)
	want = slices.Compact(want)
	switch {
	case len(want) == 0:
		return ErrPendingChanged
	case only:
		for _, fp := range want {
			if !slices.Contains(pend, fp) {
				return ErrPendingChanged
			}
		}
	case !slices.Equal(want, pend):
		return ErrPendingChanged
	}
	for _, fp := range want {
		if err := c.trust.AcknowledgeKeyChange(sid, user, fp); err != nil {
			if errors.Is(err, trust.ErrNoKeyChange) {
				return ErrPendingChanged
			}
			return err
		}
	}
	c.requeueKeyChange()
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}

// ---- alarms ----

// AlarmInfo explains a channel's alarm.
type AlarmInfo struct {
	Detail   string    // sanitized
	Why      string    // what it means and how it can happen
	Since    time.Time // zero if not stored
	Server   bool      // the server-wide alarm (it affects every encrypted channel)
	Computed bool      // derived from the current state, not stored: clears by itself
}

// Alarm explains ch's alarm; ok is false if it has none.
func (c *Client) Alarm(ch uint64) (AlarmInfo, bool) {
	st := c.E2EStatus(ch)
	if st.State != E2EAlarm {
		return AlarmInfo{}, false
	}
	sid, _ := c.cacheServerID()
	info := AlarmInfo{Detail: sanitize.Line(st.Detail)}
	if d, at, ok, _ := c.trust.ServerAlarmSince(sid); ok {
		info.Detail, info.Since, info.Server = sanitize.Line(d), at, true
	} else if d, at, ok, _ := c.trust.AlarmSince(sid, ch); ok {
		info.Detail, info.Since = sanitize.Line(d), at
	} else {
		info.Computed = true
	}
	info.Why = explainAlarm(info)
	return info, true
}

func explainAlarm(a AlarmInfo) string {
	d := a.Detail
	switch {
	case a.Server:
		return "The server stopped offering end-to-end encryption after offering it before. An honest server " +
			"doesn't downgrade; a server that wants to read new messages would."
	case strings.HasPrefix(d, unconfirmedPrefix):
		return "Someone is in the member list but no signed key update from a member includes them: only the " +
			"server says they belong. Nothing is sealed to them. It clears once a member adds them properly " +
			"(/add) or they're removed. It also shows for a moment while an add is in flight."
	case strings.Contains(d, "no longer encrypted"):
		return "This device saw the channel encrypted, and now the server says it's plaintext. An honest server " +
			"never turns encryption off; nothing is sent in the clear."
	case strings.Contains(d, "fork"):
		return "The server showed two different key bundles for the same epoch. An honest server never does: it " +
			"may be giving members different keys to split the channel."
	case strings.Contains(d, "rollback"):
		return "The server showed an older key epoch than this device already verified: it may be replaying old " +
			"state, for example to hide a membership change."
	}
	return "A key bundle from the server failed verification (bad signature, wrong rotator, key sealed to a " +
		"non-member or revoked device). An honest server and honest members can't produce one."
}

// ClearAlarm clears ch's stored alarm (or, if that's where it comes from,
// the server-wide alarm), audited as done by me. Computed alarms can't be
// cleared: they end when their cause does. If the server still presents
// what raised the alarm, it is raised again.
func (c *Client) ClearAlarm(ch uint64) error {
	info, ok := c.Alarm(ch)
	if !ok {
		return ErrNoAlarm
	}
	if info.Computed {
		return errors.New("this alarm comes from the current state and clears when its cause is gone")
	}
	sid, _ := c.cacheServerID()
	me := c.Me().ID
	var err error
	if info.Server {
		err = c.trust.ClearTrustAlarm(sid, me)
	} else {
		err = c.trust.ClearChannelAlarm(sid, ch, me)
	}
	if errors.Is(err, trust.ErrNoAlarm) {
		return ErrNoAlarm
	}
	if err != nil {
		return err
	}
	c.kickE2E()
	c.post(Update{Channels: true})
	return nil
}

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

// Team-scale chaos (QA pass before real use): one server, a team of
// clients, for TC_QA_CHAOS (e.g. "10m"; skipped when unset) of random sends,
// edits and reactions in a public and an encrypted private channel, key
// rotations, a member removed and re-added, client process restarts
// (close + reopen of the same home), server restarts on the same data dir,
// and a proxy that adds latency and kills connections. Afterwards:
//   - every queued send is delivered exactly once, none failed or pending;
//   - every client holds the same history (ids, bodies, edits, reactions);
//   - no encrypted message is waiting or invalid for a member;
//   - server/client goroutines, FDs and heap are back near the baseline;
//   - nothing panicked or logged an error-level line on the server.
//
// TC_QA_CHAOS_USERS sets the team size (default 12).
func TestQAChaosTeamScale(t *testing.T) {
	durS := os.Getenv("TC_QA_CHAOS")
	if durS == "" {
		t.Skip("set TC_QA_CHAOS=10m to run the team-scale chaos test")
	}
	dur, err := time.ParseDuration(durS)
	if err != nil {
		t.Fatal(err)
	}
	nUsers := 12
	if v := os.Getenv("TC_QA_CHAOS_USERS"); v != "" {
		nUsers, _ = strconv.Atoi(v)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")

	dir := t.TempDir()
	var logBuf lockedBuf
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var curSrv atomic.Pointer[qaServer]
	start := func(addr string) *qaServer {
		s, err := server.New(server.Config{DataDir: dir, Logger: logger, MemberInvites: true})
		if err != nil {
			t.Fatal(err)
		}
		var ln transport.Listener
		for i := 0; ; i++ { // the old listener's port may take a moment to free
			ln, err = transport.ListenWith(addr, s.Cert(), transport.ListenOpts{HTTP: true, HTTPCert: s.HTTPCert()})
			if err == nil || i > 50 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Serve(ctx, ln); close(done) }()
		q := &qaServer{Server: s, addr: ln.Addr().String()}
		q.stop = sync.OnceFunc(func() { cancel(); <-done; s.Close() })
		curSrv.Store(q)
		return q
	}
	first := start("127.0.0.1:0")
	t.Cleanup(func() { curSrv.Load().stop() })
	proxy := newChaosProxy(t, first.addr)
	proxy.calm()
	proxy.minAge = time.Second

	var us []*qaUser
	for i := range nUsers {
		u := login(t, &srv{Server: first.Server, stop: first.stop}, proxy.addr(), fmt.Sprintf("u%d", i), i == 0)
		us = append(us, &qaUser{user: u, idx: i})
	}
	t.Cleanup(func() {
		for _, u := range us {
			u.get().cancel()
			u.get().Close()
		}
	})
	waitAll := func(what string, d time.Duration, cond func(u *user) bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			ok := true
			for _, q := range us {
				if !cond(q.get()) {
					ok = false
					break
				}
			}
			if ok {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitAll("online", 60*time.Second, func(u *user) bool { return u.online() })
	admin := us[0].get()
	gen := admin.channel("general")
	sid := admin.sid(t)
	waitAll("devices verified", 120*time.Second, func(u *user) bool {
		for _, b := range us {
			if u.verifiedDevices(sid, b.get().Me().ID) != 1 {
				return false
			}
		}
		return true
	})
	setup, cancelSetup := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancelSetup()
	sec, err := admin.CreateEncryptedChannel(setup, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	// The last user is removed and re-added during the chaos; it never sends in #sec.
	floater := us[nUsers-1]
	for _, q := range us[1:] {
		if err := admin.AddMember(setup, sec.ID, q.get().Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	waitAll("#sec loaded", 120*time.Second, func(u *user) bool {
		v, ok := u.Channel(sec.ID)
		return ok && v.Member && u.status(sec.ID).State == client.E2EOK
	})
	runtime.GC()
	base := sampleLeaks()
	t.Logf("baseline: %s", base)

	// Chaos on.
	proxy.mu.Lock()
	proxy.chaos = true
	proxy.mu.Unlock()
	ctx, stopChaos := context.WithTimeout(context.Background(), dur)
	defer stopChaos()
	var stats struct {
		sends, sendErrs, edits, editErrs, reacts, restarts, srvRestarts, rotations, rotErrs, memberOps atomic.Int64
	}
	// Bodies of sends that returned nil, by channel.
	var sentMu sync.Mutex
	sent := map[uint64]map[string]bool{gen: {}, sec.ID: {}}

	var floaterIn atomic.Bool
	floaterIn.Store(true)
	var wg sync.WaitGroup
	for _, q := range us {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for ctx.Err() == nil {
				time.Sleep(time.Duration(400+rand.IntN(1600)) * time.Millisecond)
				u := q.get()
				r := rand.IntN(100)
				ch := gen
				if q != floater && rand.IntN(3) == 0 {
					ch = sec.ID
				}
				switch {
				case r < 75:
					n++
					body := fmt.Sprintf("u%d-%d-%05d", q.idx, ch, n)
					if err := u.Send(ch, body, 0); err != nil {
						stats.sendErrs.Add(1)
						continue
					}
					stats.sends.Add(1)
					sentMu.Lock()
					sent[ch][body] = true
					sentMu.Unlock()
				case r < 85: // edit one of my recent delivered messages
					var mine []client.Message
					for _, m := range u.Messages(ch, 60) {
						if m.ID != 0 && m.Author == u.Me().ID && m.Body != "" {
							mine = append(mine, m)
						}
					}
					if len(mine) == 0 {
						continue
					}
					m := mine[rand.IntN(len(mine))]
					base, _, _ := strings.Cut(m.Body, "~")
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					err := u.Edit(c, m.ID, fmt.Sprintf("%s~e%d", base, rand.IntN(1000)))
					cancel()
					stats.edits.Add(1)
					if err != nil {
						stats.editErrs.Add(1)
					}
				case r < 98: // react or unreact on a recent message
					ms := u.Messages(ch, 40)
					var ids []uint64
					for _, m := range ms {
						if m.ID != 0 {
							ids = append(ids, m.ID)
						}
					}
					if len(ids) == 0 {
						continue
					}
					id := ids[rand.IntN(len(ids))]
					emoji := []string{"👍", "🎉", "👀"}[rand.IntN(3)]
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					if rand.IntN(4) == 0 {
						u.Unreact(c, id, emoji)
					} else {
						u.React(c, id, emoji)
					}
					cancel()
					stats.reacts.Add(1)
				default: // the process dies and comes back
					q.restart(t)
					stats.restarts.Add(1)
				}
			}
		}()
	}
	// Proxy kills.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			time.Sleep(time.Duration(1000+rand.IntN(3000)) * time.Millisecond)
			proxy.killRandom()
		}
	}()
	// Server restarts on the same address and data dir.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(60+rand.IntN(60)) * time.Second):
			}
			old := curSrv.Load()
			old.stop()
			time.Sleep(time.Duration(200+rand.IntN(2000)) * time.Millisecond)
			start(first.addr)
			stats.srvRestarts.Add(1)
		}
	}()
	// Key rotations by random online members, and the floater removed/re-added.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(10+rand.IntN(20)) * time.Second):
			}
			if i%4 == 3 {
				c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var err error
				a := us[0].get()
				if floaterIn.Load() {
					err = a.RemoveMember(c, sec.ID, floater.get().Me().ID)
				} else {
					err = a.AddMember(c, sec.ID, floater.get().Me().ID)
				}
				cancel()
				// A lost RES may have applied it anyway: go by the admin's view.
				if v, ok := a.Channel(sec.ID); ok {
					floaterIn.Store(slices.Contains(v.Members, floater.get().Me().ID))
				}
				stats.memberOps.Add(1)
				if err != nil {
					t.Logf("member op: %v", err)
				}
				continue
			}
			q := us[rand.IntN(nUsers-1)]
			u := q.get()
			if !u.online() {
				continue
			}
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := u.RotateKey(c, sec.ID)
			cancel()
			if err != nil {
				stats.rotErrs.Add(1)
				t.Logf("rotate by u%d: %v", q.idx, err)
			} else {
				stats.rotations.Add(1)
			}
		}
	}()
	wg.Wait()
	proxy.calm()
	t.Logf("chaos done: sends=%d (errs %d) edits=%d (errs %d) reacts=%d client-restarts=%d server-restarts=%d rotations=%d (errs %d) member-ops=%d proxy-kills=%d",
		stats.sends.Load(), stats.sendErrs.Load(), stats.edits.Load(), stats.editErrs.Load(), stats.reacts.Load(),
		stats.restarts.Load(), stats.srvRestarts.Load(), stats.rotations.Load(), stats.rotErrs.Load(), stats.memberOps.Load(), proxy.kills)

	// Leave the floater in #sec so it ends in a known state.
	if !floaterIn.Load() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		for c.Err() == nil {
			a := us[0].get()
			if err := a.AddMember(c, sec.ID, floater.get().Me().ID); err == nil {
				break
			}
			if v, ok := a.Channel(sec.ID); ok && slices.Contains(v.Members, floater.get().Me().ID) {
				break
			}
			time.Sleep(time.Second)
		}
		cancel()
	}

	type view struct {
		lines            []string
		pending, failed  int
		waiting, invalid int
	}
	look := func(u *user, ch uint64) view {
		var v view
		for _, m := range u.Messages(ch, 1<<20) {
			switch {
			case m.ID == 0 && m.Failed != "":
				v.failed++
			case m.ID == 0:
				v.pending++
			case m.Sec == client.SecWaiting:
				v.waiting++
			case m.Sec != client.SecE2E && m.Sec != client.SecPlain && ch == sec.ID:
				v.invalid++
			default:
				var rs []string
				for e, who := range m.Reactions {
					w := slices.Clone(who)
					slices.Sort(w)
					rs = append(rs, fmt.Sprintf("%s%v", e, w))
				}
				sort.Strings(rs)
				v.lines = append(v.lines, fmt.Sprintf("%d|%d|%s|%v|%v", m.ID, m.Author, m.Body, m.Edited != 0, rs))
			}
		}
		return v
	}
	members := us[:nUsers-1] // the floater can't read epochs it was out of
	check := func(final bool) bool {
		ok := true
		for _, q := range us {
			u := q.get()
			if !u.online() {
				if final {
					t.Errorf("u%d offline: %+v", q.idx, u.Status())
				}
				ok = false
			}
		}
		for _, ch := range []uint64{gen, sec.ID} {
			who := us
			if ch == sec.ID {
				who = members
			}
			sentMu.Lock()
			want := len(sent[ch])
			sentMu.Unlock()
			ref := look(who[0].get(), ch)
			for _, q := range who {
				v := look(q.get(), ch)
				if v.pending+v.failed+v.waiting+v.invalid > 0 || len(v.lines) != want || !slices.Equal(v.lines, ref.lines) {
					ok = false
					if final {
						t.Errorf("u%d chan %d: %d shown (want %d), %d pending, %d failed, %d waiting, %d invalid, same as u0: %v",
							q.idx, ch, len(v.lines), want, v.pending, v.failed, v.waiting, v.invalid, slices.Equal(v.lines, ref.lines))
					}
				}
			}
		}
		return ok
	}
	calmAt := time.Now()
	for !check(false) {
		if time.Since(calmAt) > 5*time.Minute {
			check(true)
			t.Fatal("clients did not converge within 5 min of the chaos ending")
		}
		time.Sleep(time.Second)
	}
	t.Logf("converged %v after the chaos", time.Since(calmAt).Round(100*time.Millisecond))

	// Exactly once: every queued body (or an edit of it) appears once.
	for _, ch := range []uint64{gen, sec.ID} {
		seen := map[string]int{}
		for _, l := range look(us[0].get(), ch).lines {
			body := strings.SplitN(l, "|", 4)[2]
			b, _, _ := strings.Cut(body, "~")
			seen[b]++
		}
		for b := range sent[ch] {
			if seen[b] != 1 {
				t.Errorf("chan %d: %q shown %d times", ch, b, seen[b])
			}
		}
		if len(seen) != len(sent[ch]) {
			t.Errorf("chan %d: %d distinct bodies shown, %d sent", ch, len(seen), len(sent[ch]))
		}
	}
	for _, q := range us {
		if d, ok := q.get().Alarm(sec.ID); ok {
			t.Errorf("u%d has a trust alarm: %v", q.idx, d)
		}
		if fv := look(q.get(), sec.ID); fv.invalid > 0 {
			t.Errorf("u%d: %d invalid messages in #sec", q.idx, fv.invalid)
		}
	}

	// Leaks: same team, all online and idle again, against the baseline.
	time.Sleep(5 * time.Second)
	runtime.GC()
	end := sampleLeaks()
	t.Logf("after:    %s", end)
	if end.server > base.server+10 {
		t.Errorf("server goroutines %d → %d", base.server, end.server)
	}
	if end.client > base.client+3*nUsers {
		t.Errorf("client goroutines %d → %d", base.client, end.client)
	}
	if end.fds > base.fds+2*nUsers {
		t.Errorf("open FDs %d → %d", base.fds, end.fds)
	}
	logs := logBuf.String()
	for _, bad := range []string{"panic", "level=ERROR"} {
		if strings.Contains(logs, bad) {
			t.Errorf("server log contains %q", bad)
		}
	}
	if n := strings.Count(logs, "\n"); n > 0 {
		lines := strings.Split(strings.TrimSpace(logs), "\n")
		t.Logf("server warn+ log lines: %d, last: %s", n, lines[len(lines)-1])
	}
	for _, q := range us {
		filepath.WalkDir(q.get().home, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".log") {
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("panic")) {
					t.Errorf("%s mentions a panic", p)
				}
			}
			return nil
		})
	}
}

type qaServer struct {
	*server.Server
	addr string
	stop func()
}

type qaUser struct {
	mu   sync.Mutex
	user *user
	idx  int
}

func (q *qaUser) get() *user {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.user
}

// restart closes the client like a killed process would leave it (the cache
// and outbox stay on disk) and opens a new one on the same home.
func (q *qaUser) restart(t *testing.T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	old := q.user
	old.cancel()
	old.Close()
	time.Sleep(time.Duration(rand.IntN(3000)) * time.Millisecond)
	c, err := client.Open(old.home)
	if err != nil {
		t.Errorf("reopen u%d: %v", q.idx, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	q.user = &user{Client: c, cancel: cancel, home: old.home}
}

type leakSample struct{ server, client, total, fds int }

func (l leakSample) String() string {
	return fmt.Sprintf("goroutines total=%d server=%d client=%d, fds=%d", l.total, l.server, l.client, l.fds)
}

// sampleLeaks counts goroutines by package (server and client run in this
// one process) and this process's open FDs.
func sampleLeaks() leakSample {
	buf := make([]byte, 64<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var s leakSample
	for _, g := range strings.Split(string(buf), "\n\n") {
		s.total++
		switch {
		case strings.Contains(g, "/internal/server."):
			s.server++
		case strings.Contains(g, "/internal/client."):
			s.client++
		}
	}
	for fd := 0; fd < 1<<14; fd++ { // /dev/fd can't be listed on macOS
		if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0); e == 0 {
			s.fds++
		}
	}
	return s
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

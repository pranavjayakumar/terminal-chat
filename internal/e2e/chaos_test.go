package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sort"
	"sync"
	"testing"
	"time"
)

// chaosProxy forwards TCP connections to upstream, adds latency, and kills
// random live connections while chaos is on.
type chaosProxy struct {
	ln       net.Listener
	upstream string

	mu    sync.Mutex
	conns map[net.Conn]proxied
	kills int
	chaos bool
	// minAge spares connections younger than this from killRandom, so a
	// reconnect can get through sync even when a loaded runner makes that
	// take longer than the kill interval.
	minAge time.Duration
}

type proxied struct {
	up   net.Conn
	born time.Time
}

func newChaosProxy(t *testing.T, upstream string) *chaosProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &chaosProxy{ln: ln, upstream: upstream, conns: map[net.Conn]proxied{}, chaos: true}
	go p.accept()
	t.Cleanup(func() { ln.Close(); p.closeAll() })
	return p
}

func (p *chaosProxy) addr() string { return p.ln.Addr().String() }

func (p *chaosProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.upstream)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c] = proxied{up, time.Now()}
		p.mu.Unlock()
		go p.pipe(c, up)
		go p.pipe(up, c)
	}
}

func (p *chaosProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.mu.Lock()
			chaos := p.chaos
			p.mu.Unlock()
			if chaos {
				time.Sleep(time.Duration(rand.IntN(15)) * time.Millisecond)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	src.Close()
	dst.Close()
	p.mu.Lock() // gone already: not a candidate for killRandom
	delete(p.conns, src)
	delete(p.conns, dst)
	p.mu.Unlock()
}

// killRandom closes one live connection pair (at least minAge old).
func (p *chaosProxy) killRandom() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.chaos {
		return
	}
	var old []net.Conn
	for c, pc := range p.conns {
		if time.Since(pc.born) >= p.minAge {
			old = append(old, c)
		}
	}
	if len(old) == 0 {
		return
	}
	c := old[rand.IntN(len(old))]
	c.Close()
	p.conns[c].up.Close()
	delete(p.conns, c)
	p.kills++
}

func (p *chaosProxy) calm() {
	p.mu.Lock()
	p.chaos = false
	p.mu.Unlock()
}

func (p *chaosProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c, pc := range p.conns {
		c.Close()
		pc.up.Close()
	}
}

// Many concurrent senders through a connection-killing, latency-adding proxy:
// every client must converge on exactly the same messages, none lost or
// duplicated, and every outbox must drain.
func TestChaosConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test is slow")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	proxy := newChaosProxy(t, addr)

	const nUsers, perUser = 3, 40
	var users []*user
	for i := range nUsers {
		users = append(users, login(t, s, proxy.addr(), fmt.Sprintf("u%d", i), i == 0))
	}
	eventually(t, "all online", func() bool {
		for _, u := range users {
			if !u.online() {
				return false
			}
		}
		return true
	})
	gen := users[0].channel("general")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			time.Sleep(time.Duration(150+rand.IntN(350)) * time.Millisecond)
			proxy.killRandom()
		}
	}()

	var wg sync.WaitGroup
	for ui, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perUser {
				if err := u.Send(gen, fmt.Sprintf("u%d-%03d", ui, i), 0); err != nil {
					t.Errorf("send: %v", err)
				}
				time.Sleep(time.Duration(rand.IntN(40)) * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	// Keep killing for a while after the last send, then let things settle.
	time.Sleep(2 * time.Second)
	cancel()
	proxy.calm()
	t.Logf("proxy killed %d connections", proxy.kills)
	if proxy.kills < 5 {
		t.Fatalf("chaos too gentle: only %d kills", proxy.kills)
	}

	want := nUsers * perUser
	bodies := func(u *user) []string {
		var out []string
		for _, m := range u.Messages(gen, 1000) {
			if m.ID != 0 {
				out = append(out, fmt.Sprintf("%d:%s", m.ID, m.Body))
			}
		}
		return out
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		ok := true
		for _, u := range users {
			pending := 0
			for _, m := range u.Messages(gen, 1000) {
				if m.ID == 0 {
					pending++
				}
			}
			if pending > 0 || len(bodies(u)) != want || !u.online() {
				ok = false
			}
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			for _, u := range users {
				t.Logf("%s: %d delivered, status %+v", u.Me().Name, len(bodies(u)), u.Status())
			}
			t.Fatal("clients did not converge")
		}
		time.Sleep(100 * time.Millisecond)
	}

	ref := bodies(users[0])
	texts := map[string]int{}
	for _, b := range ref {
		var id uint64
		var text string
		fmt.Sscanf(b, "%d:%s", &id, &text)
		texts[text]++
	}
	for text, n := range texts {
		if n != 1 {
			t.Errorf("%q appears %d times", text, n)
		}
	}
	if len(texts) != want {
		t.Errorf("distinct messages %d, want %d", len(texts), want)
	}
	for _, u := range users[1:] {
		got := bodies(u)
		if fmt.Sprint(got) != fmt.Sprint(ref) {
			t.Errorf("%s diverged from %s", u.Me().Name, users[0].Me().Name)
		}
	}
	// Per-sender order must be preserved (outbox sends oldest first).
	for ui := range users {
		var mine []string
		for _, b := range ref {
			var id uint64
			var text string
			fmt.Sscanf(b, "%d:%s", &id, &text)
			if len(text) > 3 && text[:3] == fmt.Sprintf("u%d-", ui) {
				mine = append(mine, text)
			}
		}
		if !sort.StringsAreSorted(mine) {
			t.Errorf("u%d's messages arrived out of order: %v", ui, mine)
		}
	}
}

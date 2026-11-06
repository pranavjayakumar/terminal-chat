package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slowProxy forwards TCP to target, the server's side at about rate bytes/s.
func slowProxy(t *testing.T, target string, rate int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			go func() { io.Copy(u, c); u.Close() }()
			go func() {
				buf := make([]byte, rate/10)
				for {
					n, err := u.Read(buf)
					if n > 0 {
						if _, err := c.Write(buf[:n]); err != nil {
							break
						}
						time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
					}
					if err != nil {
						break
					}
				}
				c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// QA v0.6.4: on a slow link, a computer's first sync took longer than tc
// send's 15 s connect wait, so every tc send / tc tail gave up with "can't
// reach the server … may be down or blocked by a firewall" and the next run
// started the sync over: it never worked. A sync in progress now gets time.
func TestSendFreshSyncSlowLink(t *testing.T) {
	srv := startTestServer(t)
	aliceHome := loginHome(t, srv, "alice")
	t.Setenv("TC_HOME", aliceHome)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	big := strings.Repeat("x", 1500)
	c, err := connected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	general, err := resolve(ctx, c, "#general")
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		if _, err := c.SendLocal(general, big, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()

	// bob's first sync carries ~45 KB of #general: ~6 s at 8 KB/s.
	bobHome := loginHome(t, srv, "bob")
	cfgPath := filepath.Join(bobHome, "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["addr"] = slowProxy(t, cfg["addr"].(string), 8<<10)
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(a, b time.Duration) { connectWait, syncWait = a, b }(connectWait, syncWait)
	connectWait = 2 * time.Second
	syncWait = 40 * time.Second
	t.Setenv("TC_HOME", bobHome)
	if err := postTo(ctx, "#general", "made it"); err != nil {
		t.Fatalf("tc send over a slow link: %v", err)
	}
}

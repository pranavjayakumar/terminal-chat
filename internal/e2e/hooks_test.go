package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/transport"
)

func TestWebhook(t *testing.T) {
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", false)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() && alice.channel("general") != 0 })
	gen := alice.channel("general")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url, curl, err := alice.CreateHook(ctx, gen, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "https://"+addr+"/hooks/") || !strings.Contains(curl, url) {
		t.Fatalf("url %q curl %q", url, curl)
	}

	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: transport.HTTPSConfig(s.HTTPPin())}}
	resp, err := hc.Post(url, "application/json", strings.NewReader(`{"text":"build #7 passed"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST: %d %s", resp.StatusCode, body)
	}

	eventually(t, "bob sees the bot message", func() bool {
		for _, m := range bob.Messages(gen, 50) {
			if m.Body == "build #7 passed" {
				a := bob.User(m.Author)
				return a.Name == "ci-bot" && a.Bot
			}
		}
		return false
	})

	hooks, err := bob.ListHooks(ctx)
	if err != nil || len(hooks) != 1 || hooks[0].Name != "ci-bot" {
		t.Fatalf("ListHooks: %v %+v", err, hooks)
	}
	// Any member of the channel may revoke it.
	if err := bob.DeleteHook(ctx, hooks[0].ID); err != nil {
		t.Fatal(err)
	}
	resp, err = hc.Post(url, "text/plain", strings.NewReader("again"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
}

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/agent"
	"github.com/skailama/tc/internal/agent/home"
)

func TestAgentHookNeverExits2(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	err := agentHook([]string{"no-such-hook", "--dir", dir}, dir, strings.NewReader("{}"), &out, &errb)
	var code exitCode
	if !errors.As(err, &code) || code != 1 || !strings.Contains(errb.String(), "tc agent hook:") {
		t.Fatalf("err %v stderr %q", err, errb.String())
	}
	err = agentHook([]string{"stop", "--bogus"}, dir, strings.NewReader("{}"), &out, &errb)
	if !errors.As(err, &code) || code != 1 {
		t.Fatalf("bad flag: %v", err)
	}
	if err := agentHook(nil, dir, nil, &out, &errb); !errors.Is(err, errUsage) {
		t.Fatalf("no name: %v", err)
	}
}

func setupAgentHome(t *testing.T) (tcHome, dir string) {
	t.Helper()
	tcHome = t.TempDir()
	dir = home.Dir(tcHome)
	cfg := home.DefaultConfig()
	cfg.Handle, cfg.Icon = "rocky", "🦝"
	if err := home.SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	return tcHome, dir
}

func TestAgentPauseResume(t *testing.T) {
	_, dir := setupAgentHome(t)
	var out bytes.Buffer
	if err := agentPause(&out, dir, true); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := home.LoadConfig(dir); !cfg.Paused || !strings.Contains(out.String(), "paused") {
		t.Fatalf("not paused: %q", out.String())
	}
	if err := agentPause(&out, dir, false); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := home.LoadConfig(dir); cfg.Paused {
		t.Fatal("still paused")
	}
	if err := agentPause(&out, t.TempDir(), true); err == nil {
		t.Fatal("paused an agent that doesn't exist")
	}
}

func TestAgentStatusAndLogs(t *testing.T) {
	tcHome, dir := setupAgentHome(t)
	state := home.PathsFor(dir).State
	os.MkdirAll(state, 0o700)
	l, _ := agent.OpenLedger(state)
	now := time.Now()
	l.Add(agent.Spend{T: now.UnixMilli(), USD: 1.25, Kind: "ask"})
	os.WriteFile(filepath.Join(state, agent.ActivityFile), []byte(
		`{"t":1792850000000,"who":"bob","where":"#general","what":"reply","detail":"the deploy is \u001b[31mgreen","usd":0.25}`+"\n"), 0o600)
	var out bytes.Buffer
	if err := agentStatus(&out, tcHome, now); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"@rocky 🦝 · mode talk", "$1.25 of $20.00", "doesn't live on this computer", "not running"} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q:\n%s", want, s)
		}
	}
	out.Reset()
	if err := agentLogs(&out, dir, 10); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "reply") || !strings.Contains(s, "bob in #general") || strings.Contains(s, "\x1b") || !strings.Contains(s, "($0.25)") {
		t.Fatalf("logs %q", s)
	}
}

func TestHomeHere(t *testing.T) {
	tcHome, dir := setupAgentHome(t)
	if agent.HomeHere(tcHome) {
		t.Fatal("no device login yet")
	}
	dev := home.PathsFor(dir).DeviceHome
	os.MkdirAll(dev, 0o700)
	os.WriteFile(filepath.Join(dev, "config.json"), []byte(`{}`), 0o600)
	id, err := agent.MachineID(tcHome)
	if err != nil || len(id) != 32 {
		t.Fatalf("id %q %v", id, err)
	}
	if again, _ := agent.MachineID(tcHome); again != id {
		t.Fatal("machine id changed")
	}
	cfg, _ := home.LoadConfig(dir)
	cfg.HomeMachine = "someone-else"
	home.SaveConfig(dir, cfg)
	if agent.HomeHere(tcHome) {
		t.Fatal("runs on another machine's agent")
	}
	cfg.HomeMachine = id
	home.SaveConfig(dir, cfg)
	if !agent.HomeHere(tcHome) {
		t.Fatal("home machine not recognised")
	}
}

func TestServerHosts(t *testing.T) {
	if h := serverHosts("chat.example.com:7443"); len(h) != 1 || h[0] != "chat.example.com" {
		t.Fatal(h)
	}
	if serverHosts("garbage") != nil {
		t.Fatal("garbage host")
	}
}

func TestRenameDevice(t *testing.T) {
	dev := t.TempDir()
	os.WriteFile(filepath.Join(dev, "config.json"), []byte(`{"addr":"h:1","pin":"p","user_id":7,"name":"alice","agent":"rocky","key_fp":"SHA256:x"}`), 0o600)
	if deviceHandle(dev, 7) != "rocky" || deviceHandle(dev, 8) != "" || deviceHandle(t.TempDir(), 7) != "" {
		t.Fatal("deviceHandle")
	}
	if err := renameDevice(dev, "bolt"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dev, "config.json"))
	if deviceHandle(dev, 7) != "bolt" || !strings.Contains(string(b), `"key_fp": "SHA256:x"`) {
		t.Fatalf("config %s", b)
	}
}

package home

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Hook names, as passed to `tc agent hook <name>`.
const (
	HookSessionStart = "session-start"
	HookStop         = "stop"
	HookSessionEnd   = "session-end"
	HookPreCompact   = "pre-compact"
)

type hookCmd struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type hookGroup struct {
	Hooks []hookCmd `json:"hooks"`
}

type settings struct {
	Hooks       map[string][]hookGroup `json:"hooks"`
	Permissions permissions            `json:"permissions"`
	Sandbox     sandbox                `json:"sandbox"`
}

type permissions struct {
	Deny                         []string `json:"deny"`
	DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
}

type sandbox struct {
	Enabled                  bool       `json:"enabled"`
	FailIfUnavailable        bool       `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed bool       `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool       `json:"allowUnsandboxedCommands"`
	Network                  network    `json:"network"`
	Filesystem               filesystem `json:"filesystem"`
}

type network struct {
	AllowedDomains  []string `json:"allowedDomains"`
	StrictAllowlist bool     `json:"strictAllowlist"`
}

type filesystem struct {
	AllowRead  []string `json:"allowRead"`
	DenyRead   []string `json:"denyRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
}

// defaultDomains are what work in the tc repo needs from Bash: git over
// https to GitHub and Go modules.
var defaultDomains = []string{
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"*.githubusercontent.com",
	"proxy.golang.org",
	"sum.golang.org",
	"storage.googleapis.com",
}

// secretPaths are home-relative paths no agent session may read or write.
var secretPaths = []string{
	".ssh", ".aws", ".gnupg", ".netrc", ".git-credentials", ".docker", ".kube",
	".azure", ".gcloud", ".npmrc", ".pypirc", "Library",
}

// SettingsJSON renders .claude/settings.json for the agent home at dir:
// lifecycle hooks into `tc agent hook`, deny rules that keep sessions away
// from the owner's secrets and from the files only tc or the owner may write,
// and the OS sandbox for Bash.
func SettingsJSON(dir string, o Options) ([]byte, error) {
	o, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	p := PathsFor(dir)
	hook := func(name string) []hookGroup {
		return []hookGroup{{Hooks: []hookCmd{{
			Type:    "command",
			Command: o.TCBin,
			Args:    []string{"agent", "hook", name, "--dir", dir},
			Timeout: 30,
		}}}}
	}

	homeRel := append([]string{}, secretPaths...)
	homeRel = append(homeRel, configSiblings(o.UserHome)...)
	var deny []string
	for _, r := range homeRel {
		deny = append(deny, "Read(~/"+r+")", "Read(~/"+r+"/**)", "Edit(~/"+r+")", "Edit(~/"+r+"/**)")
	}
	abs := func(path string) string { return "/" + filepath.ToSlash(path) }
	deny = append(deny,
		"Read("+abs(p.DeviceHome)+"/**)",
		"Edit("+abs(p.DeviceHome)+"/**)",
		"Edit("+abs(p.Code)+")",
		"Edit("+abs(p.Config)+")",
		"Edit("+abs(p.Soul)+")",
		"Edit("+abs(p.Me)+")",
		"Edit("+abs(filepath.Dir(p.Settings))+"/**)",
		"Edit("+abs(p.Memory)+"/**)",
		"Edit("+abs(p.State)+"/**)",
	)

	denyRead := []string{}
	for _, r := range secretPaths {
		denyRead = append(denyRead, "~/"+r)
	}
	if r := keychainDir(); r != "" {
		denyRead = append(denyRead, r)
	}
	denyRead = append(denyRead, "~/.config", p.DeviceHome)

	s := settings{
		Hooks: map[string][]hookGroup{
			"SessionStart": hook(HookSessionStart),
			"Stop":         hook(HookStop),
			"SessionEnd":   hook(HookSessionEnd),
			"PreCompact":   hook(HookPreCompact),
		},
		Permissions: permissions{Deny: deny, DisableBypassPermissionsMode: "disable"},
		Sandbox: sandbox{
			Enabled:                  true,
			FailIfUnavailable:        true,
			AutoAllowBashIfSandboxed: true,
			AllowUnsandboxedCommands: false,
			Network: network{
				AllowedDomains:  append(append([]string{}, defaultDomains...), o.ExtraDomains...),
				StrictAllowlist: true,
			},
			Filesystem: filesystem{
				AllowRead: []string{dir},
				DenyRead:  denyRead,
				AllowWrite: []string{
					"~/Library/Caches/go-build",
					"~/.cache/go-build",
					"~/go/pkg/mod",
				},
				DenyWrite: []string{p.Code, p.Config, p.Soul, p.Me, filepath.Dir(p.Settings), p.Memory, p.State, p.DeviceHome},
			},
		},
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding agent settings: %w", err)
	}
	return append(b, '\n'), nil
}

// configSiblings lists ~/.config/* except tc, and ~/.config/tc/* except
// agent, as home-relative paths. Deny rules cannot carve out an exception, so
// the agent's own folder stays reachable by listing everything around it.
func configSiblings(userHome string) []string {
	var out []string
	add := func(rel string, skip string) {
		ents, err := os.ReadDir(filepath.Join(userHome, rel))
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.Name() != skip {
				out = append(out, filepath.ToSlash(filepath.Join(rel, e.Name())))
			}
		}
	}
	add(".config", "tc")
	add(filepath.Join(".config", "tc"), "agent")
	sort.Strings(out)
	return out
}

func keychainDir() string {
	if _, err := os.Stat("/Library/Keychains"); err == nil {
		return "/Library/Keychains"
	}
	return ""
}

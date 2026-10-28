package home

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	Env         map[string]string      `json:"env,omitempty"`
	Hooks       map[string][]hookGroup `json:"hooks,omitempty"`
	Permissions permissions            `json:"permissions"`
	Sandbox     *sandbox               `json:"sandbox,omitempty"`
}

type permissions struct {
	Deny                         []string `json:"deny"`
	DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
	// BlockReadsOutsideWorkingDirectories refuses file-tool reads outside
	// the working directories and, for the sandboxed shell, denies reading
	// the home folder except the working directories and allowRead
	// (Claude Code 2.1.3xx; older versions ignore it and the deny lists
	// below still hold).
	BlockReadsOutsideWorkingDirectories bool `json:"blockReadsOutsideWorkingDirectories"`
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

// defaultDomains is empty: the sandboxed shell reaches only the tc server
// and the hosts the owner added. Go modules come from Paths.ModProxy, a
// cache tc fills from proxy.golang.org at work_start (outside the sandbox)
// and the sandbox uses as its GOPROXY. Not even the module proxy is open:
// asking it for attacker.example/<secret> makes it fetch that path from
// the attacker's host, which carries data out, and so do
// storage.googleapis.com (anyone's bucket) and sum.golang.org lookups.
var defaultDomains = []string{}

// secretPaths are home-relative paths no agent session may read or write.
// With blockReadsOutsideWorkingDirectories the sandbox already denies all
// of the home folder; this list is what still holds on a Claude Code too
// old to know that setting, and for the file tools.
var secretPaths = []string{
	".ssh", ".aws", ".gnupg", ".netrc", ".git-credentials", ".docker", ".kube",
	".azure", ".gcloud", ".npmrc", ".pypirc", "Library",
	".claude", ".claude.json", ".claude.json.backup",
	".zsh_history", ".bash_history", ".histfile", ".python_history", ".node_repl_history",
	".psql_history", ".mysql_history", ".sqlite_history", ".lesshst", ".viminfo",
	".local/share", ".local/state", ".vault-token", ".cargo/credentials", ".cargo/credentials.toml",
	".terraform.d", ".pgpass", ".my.cnf", ".gitconfig", ".config",
	".password-store", ".1password", ".mozilla", ".thunderbird",
}

// readOK are home-relative paths the sandboxed shell may read although
// they sit in the home folder: Go's env file. Its module and build caches
// are its own (Paths.GoCache, GoModCache), never the owner's: a shell that
// could write those could plant code the owner's next build runs.
var readOK = []string{
	"Library/Application Support/go", ".config/go",
}

// SettingsJSON renders .claude/settings.json for the agent home at dir:
// lifecycle hooks into `tc agent hook`, deny rules that keep sessions away
// from the owner's secrets and from the files only tc or the owner may write,
// and the OS sandbox for Bash.
//
// The sandboxed shell runs in worktrees/ (Work mode's working directory)
// and the folders the owner granted. It may not write anything git or tc
// later runs outside the sandbox: repo/ (the clone tc fetches and pushes
// from), any .git (a worktree's pointer file names the git dir tc's git
// would use), dream/ (the memory worktree tc merges) and tc's own files.
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
	tcHome := filepath.Dir(dir)
	abs := func(path string) string { return "/" + filepath.ToSlash(path) }

	homeRel := append([]string{}, secretPaths...)
	homeRel = append(homeRel, configSiblings(o.UserHome)...)
	var deny []string
	for _, r := range homeRel {
		if r == ".config" {
			continue // the agent home is in there: its siblings are listed instead
		}
		deny = append(deny, "Read(~/"+r+")", "Read(~/"+r+"/**)", "Edit(~/"+r+")", "Edit(~/"+r+"/**)")
	}
	for _, d := range tcSiblings(tcHome, dir) {
		deny = append(deny, "Read("+abs(d)+")", "Read("+abs(d)+"/**)", "Edit("+abs(d)+")", "Edit("+abs(d)+"/**)")
	}
	deny = append(deny,
		"Read(**/.env)", "Read(**/.env.*)",
		"Read("+abs(p.DeviceHome)+"/**)",
		"Read("+abs(p.State)+"/**)",
		"Edit("+abs(p.DeviceHome)+"/**)",
		"Edit("+abs(p.Code)+")",
		"Edit("+abs(p.Config)+")",
		"Edit("+abs(p.Soul)+")",
		"Edit("+abs(p.Me)+")",
		"Edit("+abs(filepath.Dir(p.Settings))+"/**)",
		"Edit("+abs(p.Memory)+"/**)",
		"Edit("+abs(p.State)+"/**)",
		"Edit("+abs(p.Repo)+")",
		"Edit("+abs(p.Repo)+"/**)",
		"Edit("+abs(p.Dream)+")",
		"Edit("+abs(p.Dream)+"/**)",
		"Edit(**/.git)",
		"Edit(**/.git/**)",
	)

	denyRead := []string{}
	for _, r := range secretPaths {
		denyRead = append(denyRead, "~/"+r)
	}
	if r := keychainDir(); r != "" {
		denyRead = append(denyRead, r)
	}
	// TC_HOME (cache.db holds messages in the clear) wherever it is, and
	// the agent's own private parts.
	denyRead = append(denyRead, tcHome, p.DeviceHome, p.State, p.Memory, p.Dream, p.Me, p.Config)
	allowRead := []string{p.Worktrees, p.Repo, p.Skills, filepath.Dir(p.GoCache), p.ModProxy}
	for _, r := range readOK {
		allowRead = append(allowRead, "~/"+r)
	}
	if o.GoRoot != "" {
		allowRead = append(allowRead, o.GoRoot)
	}

	denyWrite := []string{p.Code, p.Config, p.Soul, p.Me, filepath.Dir(p.Settings), p.Memory, p.State, p.DeviceHome,
		p.Repo, p.Dream, p.ModProxy, "**/.git"}
	// Linux skips glob write rules: name each worktree's .git too.
	if ents, err := os.ReadDir(p.Worktrees); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				denyWrite = append(denyWrite, filepath.Join(p.Worktrees, e.Name(), ".git"))
			}
		}
	}

	s := settings{
		Env: map[string]string{
			"GOCACHE":     p.GoCache,
			"GOMODCACHE":  p.GoModCache,
			"GOTOOLCHAIN": "local",
			"GOTELEMETRY": "off",
			"GOPROXY":     "file://" + filepath.ToSlash(filepath.Join(p.ModProxy, "cache", "download")),
			"GOWORK":      "off",
		},
		Hooks: map[string][]hookGroup{
			"SessionStart": hook(HookSessionStart),
			"Stop":         hook(HookStop),
			"SessionEnd":   hook(HookSessionEnd),
			"PreCompact":   hook(HookPreCompact),
		},
		Permissions: permissions{Deny: deny, DisableBypassPermissionsMode: "disable", BlockReadsOutsideWorkingDirectories: true},
		Sandbox: &sandbox{
			Enabled:                  true,
			FailIfUnavailable:        true,
			AutoAllowBashIfSandboxed: true,
			AllowUnsandboxedCommands: false,
			Network: network{
				AllowedDomains:  append(append([]string{}, defaultDomains...), o.ExtraDomains...),
				StrictAllowlist: true,
			},
			Filesystem: filesystem{
				AllowRead:  allowRead,
				DenyRead:   denyRead,
				AllowWrite: []string{filepath.Dir(p.GoCache)},
				DenyWrite:  denyWrite,
			},
		},
	}
	return encodeSettings(s)
}

// DreamSettingsJSON renders .claude/dream-settings.json for the memory
// dream: an edit-only run in dream/ whose input includes what others told
// the agent. It may edit notes, never the worktree's .git pointer that
// tc's git reads afterwards, and read nothing outside dream/.
func DreamSettingsJSON(dir string) ([]byte, error) {
	p := PathsFor(dir)
	abs := "/" + filepath.ToSlash(p.Dream)
	return encodeSettings(settings{Permissions: permissions{
		Deny: []string{
			"Edit(" + abs + "/.git)", "Edit(" + abs + "/.git/**)", "Edit(**/.git)", "Edit(**/.git/**)",
			"Read(" + abs + "/.git)",
		},
		DisableBypassPermissionsMode:        "disable",
		BlockReadsOutsideWorkingDirectories: true,
	}})
}

func encodeSettings(s settings) ([]byte, error) {
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

// tcSiblings lists what sits next to the agent home in TC_HOME (cache.db,
// keys, e2e.db), for a TC_HOME outside ~/.config.
func tcSiblings(tcHome, agentDir string) []string {
	ents, err := os.ReadDir(tcHome)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if full := filepath.Join(tcHome, e.Name()); full != agentDir {
			out = append(out, full)
		}
	}
	return out
}

func keychainDir() string {
	if _, err := os.Stat("/Library/Keychains"); err == nil {
		return "/Library/Keychains"
	}
	return ""
}

// goRootUnder is the Go toolchain's root when it lives in the home folder
// (the sandbox must be able to read it there), else "".
func goRootUnder(userHome string) string {
	bin, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return ""
	}
	root := filepath.Dir(filepath.Dir(bin))
	if rel, err := filepath.Rel(userHome, root); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return root
}

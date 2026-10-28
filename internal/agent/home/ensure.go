package home

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	agentassets "github.com/skailama/tc/assets/agent"
)

// Options tune the files Ensure writes. The zero value works.
type Options struct {
	// TCBin is the tc binary the hooks run. Default: this executable, else "tc".
	TCBin string
	// UserHome is the owner's home directory, used for the deny lists.
	// Default: os.UserHomeDir.
	UserHome string
	// ExtraDomains are added to the sandbox network allowlist (the tc server,
	// owner-added hosts).
	ExtraDomains []string
	// GoRoot is a Go toolchain in the home folder the sandbox may read.
	// Default: the go on PATH when it lives under UserHome.
	GoRoot string
}

const ownedManifest = ".claude/tc-owned.json"

// Ensure creates the agent home at dir with default Options.
func Ensure(dir string) error { return EnsureWith(dir, Options{}) }

// EnsureWith creates the folders of the agent home, (re)writes the files tc
// owns when their content changed, removes tc-owned files no longer shipped,
// and writes the owner's files only when they are missing. It never touches
// memory/.
func EnsureWith(dir string, o Options) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("agent home %q is not an absolute path", dir)
	}
	o, err := o.withDefaults()
	if err != nil {
		return err
	}
	p := PathsFor(dir)
	for _, d := range []string{p.Root, filepath.Dir(p.Settings), p.Skills, p.DeviceHome, p.State, p.Worktrees, filepath.Dir(p.GoCache)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
		// MkdirAll keeps the mode of a folder that was already there.
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("securing %s: %w", d, err)
		}
	}

	owned, err := ownedFiles(dir, o)
	if err != nil {
		return err
	}
	for rel, data := range owned {
		if err := writeIfChanged(filepath.Join(dir, filepath.FromSlash(rel)), data); err != nil {
			return err
		}
	}
	if err := pruneOwned(dir, owned); err != nil {
		return err
	}

	soul, err := defaultSoul()
	if err != nil {
		return err
	}
	if err := writeIfMissing(p.Soul, []byte(soul)); err != nil {
		return err
	}
	if err := writeIfMissing(p.Me, []byte(defaultMe)); err != nil {
		return err
	}
	if _, err := os.Stat(p.Config); errors.Is(err, fs.ErrNotExist) {
		if err := SaveConfig(dir, DefaultConfig()); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("checking agent config: %w", err)
	}
	return nil
}

func defaultSoul() (string, error) {
	b, err := agentassets.FS.ReadFile("soul.md")
	if err != nil {
		return "", fmt.Errorf("reading shipped soul: %w", err)
	}
	return string(b), nil
}

const defaultMe = "# About my owner\n\nNothing yet. The setup chat fills this in.\n"

func (o Options) withDefaults() (Options, error) {
	if o.TCBin == "" {
		o.TCBin = "tc"
		if exe, err := os.Executable(); err == nil {
			o.TCBin = exe
		}
	}
	if o.UserHome == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return o, fmt.Errorf("finding home directory: %w", err)
		}
		o.UserHome = h
	}
	if o.GoRoot == "" {
		o.GoRoot = goRootUnder(o.UserHome)
	}
	return o, nil
}

// ownedFiles maps slash paths relative to the agent home to their content.
func ownedFiles(dir string, o Options) (map[string][]byte, error) {
	out := map[string][]byte{}
	code, err := agentassets.FS.ReadFile("CLAUDE.md")
	if err != nil {
		return nil, fmt.Errorf("reading shipped code: %w", err)
	}
	out["CLAUDE.md"] = code
	err = fs.WalkDir(agentassets.FS, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := agentassets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		out[path.Join(".claude", p)] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading shipped skills: %w", err)
	}
	settings, err := SettingsJSON(dir, o)
	if err != nil {
		return nil, err
	}
	out[".claude/settings.json"] = settings
	dream, err := DreamSettingsJSON(dir)
	if err != nil {
		return nil, err
	}
	out[".claude/dream-settings.json"] = dream
	return out, nil
}

func pruneOwned(dir string, owned map[string][]byte) error {
	mpath := filepath.Join(dir, filepath.FromSlash(ownedManifest))
	var prev []string
	if b, err := os.ReadFile(mpath); err == nil {
		_ = json.Unmarshal(b, &prev)
	}
	for _, rel := range prev {
		if _, ok := owned[rel]; ok || !safeRel(rel) {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing old %s: %w", rel, err)
		}
		_ = os.Remove(filepath.Dir(full)) // only succeeds when the skill folder is now empty
	}
	names := make([]string, 0, len(owned))
	for rel := range owned {
		names = append(names, rel)
	}
	sort.Strings(names)
	b, _ := json.MarshalIndent(names, "", "  ")
	return writeIfChanged(mpath, append(b, '\n'))
}

// safeRel keeps a tampered manifest from deleting anything outside the
// tc-owned files.
func safeRel(rel string) bool {
	c := path.Clean(rel)
	if c != rel || path.IsAbs(c) || c == ".." || len(c) >= 3 && c[:3] == "../" {
		return false
	}
	return c == "CLAUDE.md" || c == ".claude/settings.json" || c == ".claude/dream-settings.json" || (len(c) > len(".claude/skills/") && c[:len(".claude/skills/")] == ".claude/skills/")
}

func writeIfChanged(p string, data []byte) error {
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	return WriteFileAtomic(p, data, 0o600)
}

func writeIfMissing(p string, data []byte) error {
	_, err := os.Stat(p)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", p, err)
	}
	return WriteFileAtomic(p, data, 0o600)
}

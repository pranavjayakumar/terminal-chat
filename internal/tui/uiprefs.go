package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Sidebar sizes. sbAuto is never stored: it means "no choice made yet".
const (
	sbNormal = "normal"
	sbWide   = "wide"
	sbIcons  = "icons"
	sbHidden = "hidden" // only as a narrow terminal's session choice (Ctrl-B)
)

// uiPrefs are this device's look-and-feel settings, kept in TC_HOME/ui.json.
// Only the UI reads or writes the file; the server never sees it.
type uiPrefs struct {
	Theme      string `json:"theme,omitempty"`
	Compact    bool   `json:"compact,omitempty"`
	Sidebar    string `json:"sidebar,omitempty"`
	NoMotion   bool   `json:"no_motion,omitempty"`
	path       string
	loadFailed bool
}

func loadUIPrefs(path string) uiPrefs {
	p := uiPrefs{path: path}
	if path == "" {
		return p
	}
	b, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(b, &p) != nil {
			p = uiPrefs{path: path, loadFailed: true}
		}
	}
	if _, ok := themes[p.Theme]; !ok {
		p.Theme = ""
	}
	switch p.Sidebar {
	case sbNormal, sbWide, sbIcons:
	default:
		p.Sidebar = ""
	}
	return p
}

// save writes the file atomically. It is a few bytes on local disk, so it
// runs inline like the drafts cache.
func (p uiPrefs) save() error {
	if p.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".ui-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), p.path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

func (p uiPrefs) themeName() string {
	if p.Theme == "" {
		return defaultTheme
	}
	return p.Theme
}

func (p uiPrefs) sidebarMode() string {
	if p.Sidebar == "" {
		return sbNormal
	}
	return p.Sidebar
}

// savePrefs stores m.ui, reporting a failure once as a notice.
func (m *Model) savePrefs() {
	if err := m.ui.save(); err != nil {
		m.setErr("save settings", err)
	}
}

// setTheme switches the theme, cross-fading from the old one.
func (m *Model) setTheme(name string) {
	th, ok := themes[name]
	if !ok || th == m.st.th {
		return
	}
	m.fx.themeFrom(m)
	m.applyStyles(newStyles(m.st.color, m.st.prof, th))
	m.ui.Theme = name
	if name == defaultTheme {
		m.ui.Theme = ""
	}
	m.savePrefs()
	m.setInfo("◐ theme · " + name)
}

// applyStyles puts st everywhere a copy of the old styles lives.
func (m *Model) applyStyles(st *styles) {
	m.st = st
	m.r.st = st
	m.blocks.reset()
	m.main.ver++
	m.thread.ver++
	styleInput(&m.input, st)
	styleLine(&m.line, st)
}

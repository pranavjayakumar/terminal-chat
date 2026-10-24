package home

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Mode decides who may make the agent use the shell and edit files.
type Mode string

const (
	ModeTalk Mode = "talk" // answers only
	ModeWork Mode = "work" // the owner's asks may work in granted folders
	ModeCrew Mode = "crew" // anyone's asks may work in granted folders
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeTalk || m == ModeWork || m == ModeCrew }

// Access is what the owner lets the agent reach beyond its home and the
// places it is called.
type Access struct {
	Folders []string `json:"folders"` // absolute paths granted with --add-dir in Work/Crew
	Chans   []uint64 `json:"chans"`   // channels it may read for anyone's asks
	DMs     bool     `json:"dms"`     // the owner's asks may read the owner's DMs
}

// MCPServer is an MCP server the owner added. It uses the field names of a
// Claude Code --mcp-config entry so it can be passed through unchanged.
type MCPServer struct {
	Type    string            `json:"type,omitempty"` // stdio (default), http, sse
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Config is config.json in the agent home. The owner edits it from the panel.
type Config struct {
	Handle        string  `json:"handle"`
	Icon          string  `json:"icon"`
	Mode          Mode    `json:"mode"`
	DailyCapUSD   float64 `json:"daily_cap_usd"`
	RunCapUSD     float64 `json:"run_cap_usd"`
	AsksPerPerson int     `json:"asks_per_person"` // per person per day
	RolloverPct   int     `json:"rollover_pct"`    // % of the context window
	IdleMinutes   int     `json:"idle_minutes"`    // stop Claude after this long idle
	// EngageMinutes keeps the agent in a thread after it replies: new
	// messages there reach it without an @mention for this long (0 = off).
	EngageMinutes int                  `json:"engage_minutes"`
	Access        Access               `json:"access"`
	MCPServers    map[string]MCPServer `json:"mcp_servers,omitempty"`
	HomeMachine   string               `json:"home_machine"` // machine id the agent runs on
	Paused        bool                 `json:"paused"`
}

// DefaultConfig is the config a new agent starts with.
func DefaultConfig() Config {
	return Config{
		Mode:          ModeTalk,
		DailyCapUSD:   20,
		RunCapUSD:     2,
		AsksPerPerson: 15,
		RolloverPct:   50,
		IdleMinutes:   60,
		EngageMinutes: 10,
		Access:        Access{Folders: []string{}, Chans: []uint64{}},
	}
}

// Validate rejects values the supervisor cannot run with.
func (c Config) Validate() error {
	switch {
	case !c.Mode.Valid():
		return fmt.Errorf("mode %q: want talk, work or crew", c.Mode)
	case c.DailyCapUSD <= 0:
		return errors.New("daily_cap_usd must be above 0")
	case c.RunCapUSD <= 0:
		return errors.New("run_cap_usd must be above 0")
	case c.RunCapUSD > c.DailyCapUSD:
		return errors.New("run_cap_usd is above daily_cap_usd")
	case c.AsksPerPerson < 1:
		return errors.New("asks_per_person must be at least 1")
	case c.RolloverPct < 10 || c.RolloverPct > 95:
		return errors.New("rollover_pct must be 10..95")
	case c.EngageMinutes < 0 || c.EngageMinutes > 24*60:
		return errors.New("engage_minutes must be 0..1440")
	case c.IdleMinutes < 1:
		return errors.New("idle_minutes must be at least 1")
	}
	for _, f := range c.Access.Folders {
		if !filepath.IsAbs(f) {
			return fmt.Errorf("access folder %q is not an absolute path", f)
		}
	}
	return nil
}

// LoadConfig reads config.json from the agent home. A missing file gives the
// defaults; fields missing from the file keep their defaults.
func LoadConfig(dir string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(PathsFor(dir).Config)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading agent config: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parsing agent config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("agent config: %w", err)
	}
	return c, nil
}

// SaveConfig validates c and writes it atomically.
func SaveConfig(dir string, c Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("agent config: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding agent config: %w", err)
	}
	return WriteFileAtomic(PathsFor(dir).Config, append(b, '\n'), 0o600)
}

// WriteFileAtomic writes data to a temp file next to path and renames it in
// place, so readers never see a half-written file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

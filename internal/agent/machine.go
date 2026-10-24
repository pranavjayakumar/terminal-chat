package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/skailama/tc/internal/agent/home"
)

// MachineIDFile holds this machine's random id in TC_HOME. The agent runs
// only on the machine whose id is its config's home_machine, so two
// laptops logged in as the same person never both answer.
const MachineIDFile = "machine-id"

// MachineID returns this machine's id for tcHome, making one on first use.
func MachineID(tcHome string) (string, error) {
	p := filepath.Join(tcHome, MachineIDFile)
	b, err := os.ReadFile(p)
	if err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 32 {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	var r [16]byte
	_, _ = rand.Read(r[:])
	id := hex.EncodeToString(r[:])
	if err := home.WriteFileAtomic(p, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// HomeHere reports whether the agent of tcHome is set up and lives on this
// machine.
func HomeHere(tcHome string) bool {
	dir := home.Dir(tcHome)
	if _, err := os.Stat(filepath.Join(home.PathsFor(dir).DeviceHome, "config.json")); err != nil {
		return false
	}
	cfg, err := home.LoadConfig(dir)
	if err != nil || cfg.Handle == "" || cfg.HomeMachine == "" {
		return false
	}
	id, err := MachineID(tcHome)
	return err == nil && id == cfg.HomeMachine
}

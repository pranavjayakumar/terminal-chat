package memory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skailama/tc/internal/agent/home"
)

// Environment the supervisor sets on each claude run; the hook commands
// inherit it.
const (
	EnvSessionKey = "TC_AGENT_SESSION_KEY" // thread or DM topic key
	EnvOwnerAsk   = "TC_AGENT_OWNER_ASK"   // "1" when the owner asked
	EnvPeople     = "TC_AGENT_PEOPLE"      // comma-separated handles in the thread
	EnvAsker      = "TC_AGENT_ASKER"       // handle of whoever asked
	EnvOwner      = "TC_AGENT_OWNER"       // the owner: "Fizan (@fizan)"
)

// Raw hook lines are cut to this many bytes before they go in the daily log.
const hookLineMaxBytes = 300

// hookInput is the JSON Claude Code writes to a hook's stdin. Only the fields
// tc uses.
type hookInput struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	HookEventName        string `json:"hook_event_name"`
	Source               string `json:"source"`  // SessionStart: startup, resume, clear, compact
	Reason               string `json:"reason"`  // SessionEnd
	Trigger              string `json:"trigger"` // PreCompact: manual, auto
	LastAssistantMessage string `json:"last_assistant_message"`
}

type sessionStartOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// RunHook handles `tc agent hook <name>` for the agent home at agentDir.
//
//   - session-start prints the memory snapshot as additionalContext. It is
//     built once per Claude session and frozen: a resumed session already has
//     it, a compacted one gets the same text again.
//   - stop and session-end append the last reply to the daily log, once.
//   - pre-compact saves the tail of the transcript as the thread's checkpoint.
//
// Errors are returned for the caller to print and exit 1; exit code 2 would
// make Claude Code block the turn.
func RunHook(name string, stdin io.Reader, stdout io.Writer, agentDir string) error {
	return runHook(name, stdin, stdout, agentDir, os.Getenv, time.Now)
}

func runHook(name string, stdin io.Reader, stdout io.Writer, agentDir string, getenv func(string) string, now func() time.Time) error {
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading hook input: %w", err)
	}
	s, err := Open(agentDir)
	if err != nil {
		return err
	}
	s.now = now
	ownerAsk := getenv(EnvOwnerAsk) == "1"
	key := getenv(EnvSessionKey)
	switch name {
	case home.HookSessionStart:
		return s.hookSessionStart(in, stdout, Params{
			OwnerAsk:   ownerAsk,
			People:     splitPeople(getenv(EnvPeople)),
			SessionKey: key,
			Owner:      getenv(EnvOwner),
		})
	case home.HookStop, home.HookSessionEnd:
		return s.hookLogReply(in, getenv(EnvAsker), ownerAsk)
	case home.HookPreCompact:
		if key == "" {
			key = in.SessionID
		}
		if key == "" {
			return nil
		}
		tail := transcriptTail(in.TranscriptPath, HandoffMaxBytes-200)
		if tail == "" {
			return nil
		}
		return s.WriteCheckpoint(key, "Saved before compaction at "+now().Format("2006-01-02 15:04")+". Your last words:\n"+tail)
	default:
		return fmt.Errorf("unknown hook %q", name)
	}
}

func splitPeople(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) hookStateDir() string { return filepath.Join(home.PathsFor(s.agentDir).State, "hooks") }

func sessionFile(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:12])
}

func (s *Store) hookSessionStart(in hookInput, stdout io.Writer, p Params) error {
	if in.Source == "resume" {
		return nil
	}
	dir := s.hookStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating hook state: %w", err)
	}
	frozen := filepath.Join(dir, sessionFile(in.SessionID)+".snapshot")
	var snap string
	if b, err := os.ReadFile(frozen); err == nil && in.SessionID != "" {
		snap = string(b)
	} else {
		snap = s.Snapshot(p)
		if in.SessionID != "" {
			if err := home.WriteFileAtomic(frozen, []byte(snap), 0o600); err != nil {
				return err
			}
		}
		pruneOld(dir, s.now().Add(-7*24*time.Hour))
	}
	var out sessionStartOutput
	out.HookSpecificOutput.HookEventName = "SessionStart"
	out.HookSpecificOutput.AdditionalContext = snap
	return json.NewEncoder(stdout).Encode(out)
}

func pruneOld(dir string, before time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// hookLogReply appends the session's last reply to the daily log, skipping
// empty replies, NO_REPLY and a reply already logged by the other hook.
func (s *Store) hookLogReply(in hookInput, asker string, ownerAsk bool) error {
	msg := strings.TrimSpace(in.LastAssistantMessage)
	if msg == "" || msg == "NO_REPLY" {
		return nil
	}
	dir := s.hookStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating hook state: %w", err)
	}
	sum := sha256.Sum256([]byte(msg))
	mark := hex.EncodeToString(sum[:])
	last := filepath.Join(dir, sessionFile(in.SessionID)+".last")
	if b, err := os.ReadFile(last); err == nil && string(b) == mark {
		return nil
	}
	src := "hook"
	if asker != "" {
		src = asker
	}
	err := s.Remember(Fact{
		Subject: "replied",
		Text:    oneLine(msg, hookLineMaxBytes),
		KeepFor: KeepWeek,
		Source:  src,
		Trust:   TrustSelf,
		Private: ownerAsk,
	})
	if err != nil {
		return err
	}
	return home.WriteFileAtomic(last, []byte(mark), 0o600)
}

// transcriptTail returns the agent's last text replies from a Claude Code
// transcript (JSONL), newest last, within max bytes.
func transcriptTail(path string, max int) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var texts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Type != "assistant" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(line.Message.Content, &blocks) != nil {
			var s string
			if json.Unmarshal(line.Message.Content, &s) == nil && s != "" {
				texts = append(texts, s)
			}
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				texts = append(texts, b.Text)
			}
		}
	}
	var out []string
	size := 0
	for i := len(texts) - 1; i >= 0 && size < max; i-- {
		t := "- " + oneLine(texts[i], 600)
		if size+len(t)+1 > max {
			break
		}
		out = append([]string{t}, out...)
		size += len(t) + 1
	}
	return strings.Join(out, "\n")
}

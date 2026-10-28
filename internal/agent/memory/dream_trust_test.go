package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func breakNote(trust, private, body string) string {
	p := ""
	if private != "" {
		p = "private: true\n"
	}
	return "---\ntype: fact\nsource: x\ntrust: " + trust + "\n" + p + "modified: 2026-10-24T00:00:00Z\n---\n" + body + "\n"
}

// Two notes merged from the same day's log cite the same "- Source:" line.
// That boilerplate is treated as copied text, so a private (or lower-trust)
// note citing a day poisons every other note citing that day.
func TestDreamSharedCitationAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		base Note
		src  string
	}{
		{"private base note", Note{Path: "facts/secret.md", Body: "still a secret\n- Source: daily/2026-10-27.md", Trust: TrustOwner, Private: true}, "- Source: daily/2026-10-27.md"},
		{"lower-trust base note", Note{Path: "people/bob.md", Body: "bob says hi\n- Source: daily/2026-10-27.md", Trust: TrustOther}, "- Source: daily/2026-10-27.md"},
		{"Sources: plural", Note{Path: "facts/secret.md", Body: "still a secret\nSources: daily/2026-10-27.md", Trust: TrustOwner, Private: true}, "Sources: daily/2026-10-27.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newStore(t)
			seedNotes(t, s, 8)
			if err := s.Write(tc.base); err != nil {
				t.Fatal(err)
			}
			if err := s.Commit("seed"); err != nil {
				t.Fatal(err)
			}
			d, err := s.PrepareDream()
			if err != nil {
				t.Fatal(err)
			}
			defer s.DiscardDream(d)
			body := "the owner likes tea\n" + tc.src
			_ = os.WriteFile(filepath.Join(d.Dir, "facts", "tea.md"), []byte(breakNote("owner", "", body)), 0o600)
			if err := s.ValidateDream(d); err != nil {
				t.Fatalf("an honest new note was rejected: %v", err)
			}
		})
	}
}

// The agent answering a guest with a line of an owner note, verbatim, logs
// that line as a non-private "self" fact (rank other). From then on any
// dream that touches the owner note is refused as "lower-trust text".
func TestDreamSelfQuoteAllowed(t *testing.T) {
	s, _ := newStore(t)
	seedNotes(t, s, 8)
	const line = "Deploys happen on Thursdays at 3pm."
	if err := s.Write(Note{Path: "facts/deploy.md", Body: line, Trust: TrustOwner}); err != nil {
		t.Fatal(err)
	}
	// bob asked "when do we deploy?"; the Stop hook logged the agent's reply.
	if err := s.Remember(Fact{Subject: "replied", Text: line, Source: "bob", Trust: TrustSelf}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit("seed"); err != nil {
		t.Fatal(err)
	}
	d, err := s.PrepareDream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.DiscardDream(d)
	_ = os.WriteFile(filepath.Join(d.Dir, "facts", "deploy.md"), []byte(breakNote("owner", "", line+"\nFreeze on holidays.")), 0o600)
	if err := s.ValidateDream(d); err != nil {
		t.Fatalf("editing the owner's own note was rejected: %v", err)
	}
}

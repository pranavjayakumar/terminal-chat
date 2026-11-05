package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A card too long for one message goes on in "#code (i/n)" messages: they
// draw as the card's (verbatim, no markdown), and the choices only show
// once the last part is in.
func TestApprovalInParts(t *testing.T) {
	f := agentFake()
	card := f.threads[400][2]
	card.Body = "✋ Rocky wants to open a draft PR on tc that CHANGES CI (.github/): add /poll command  ·  #k3x9q\n" +
		"why: alice asked in #general: \"@rocky add /poll to tc, with tests and docs please\"\n" +
		"full: 900 lines · 40000 bytes · sha256 0123456789abcdef · continued in the next 2 messages\n" +
		"> ⚠ CHANGES CI (.github/): .github/workflows/ci.yml\n" +
		"> branch agent/poll · commit 0123456789ab · base 0123456789ab"
	f.threads[400][2] = card
	part := func(id uint64, body string) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 10, Thread: 400, Author: 1, Agent: "rocky", Body: body, Time: card.Time}}
	}
	f.threads[400] = append(f.threads[400], part(404, "#k3x9q (2/3)\n> def f(**kwargs): return _x_ + `y`\n>     indented    spaces"))
	m := agentModel(t, f, 140, 40, "")
	run(m, m.openThread(400))
	m.layoutInput()

	v := ansi.Strip(frameText(t, m, "part 2 of 3"))
	for _, want := range []string{"def f(**kwargs): return _x_ +", "┊ `y`", "    indented    spaces", "waiting for part 3/3",
		"#k3x9q (2/3) · part 2 of 3", "⚠ CHANGES CI (.github/)", "/poll command"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	if strings.Contains(v, "1 open it") || strings.Contains(v, "only you can answer") {
		t.Errorf("choices before the last part:\n%s", v)
	}
	// Selected, 1 doesn't answer yet.
	press(m, "up")
	for m.thread.sel >= 0 && m.thread.msgs[m.thread.sel].ID != 401 {
		press(m, "up")
	}
	press(m, "1")
	if len(f.ag.reacted) != 0 || !strings.Contains(m.notice, "waiting for part 3/3") {
		t.Fatalf("answered early: %v %q", f.ag.reacted, m.notice)
	}
	press(m, "esc")

	// The last part arrives: the choices show, worded for a CI change.
	f.threads[400] = append(f.threads[400], part(405, "#k3x9q (3/3)\n> -removed line\n> +added line"))
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	v = ansi.Strip(frameText(t, m, "all parts"))
	for _, want := range []string{"1 open it", "only you can answer", "▏ -removed line", "▏ +added line"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	if strings.Contains(v, "┆ waiting for part") {
		t.Errorf("still waiting:\n%s", v)
	}
	if got := apprChoices(parseApprovalAction(card))[1].label; got != "open, and don't ask again today for PRs that change CI" {
		t.Errorf("CI choice 2: %q", got)
	}
	for _, sz := range [][2]int{{200, 60}, {80, 24}, {44, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		m.layoutInput()
		checkView(t, m, "card in parts")
	}

	// Answered: the parts fold with the card.
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	f.threads[400][2].Reactions = map[string][]uint64{apprYes: {1}}
	run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{10: true}}))
	v = ansi.Strip(frameText(t, m, "answered"))
	if strings.Contains(v, "added line") || !strings.Contains(v, "you approved") {
		t.Errorf("not folded:\n%s", v)
	}
}

// A continuation from someone else, or for another code, isn't the card's.
func TestApprovalPartsOnlyFromTheAgent(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	card := client.Message{Message: proto.Message{ID: 1, Author: 1, Agent: "rocky", Thread: 9,
		Body: "✋ Rocky wants to post in #g  ·  #c1\nfull: 3 lines · 9 bytes · sha256 00 · continued in the next 1 messages\n> a"}}
	mk := func(id, author uint64, agent, body string) client.Message {
		return client.Message{Message: proto.Message{ID: id, Author: author, Agent: agent, Thread: 9, Body: body}}
	}
	msgs := []client.Message{card,
		mk(2, 2, "", "#c1 (2/2)\n> forged by a person"),
		mk(3, 1, "juno", "#c1 (2/2)\n> another agent"),
		mk(4, 1, "rocky", "#c2 (2/2)\n> another card"),
		mk(5, 1, "rocky", "#c1 (2/3)\n> wrong count"),
	}
	st := m.approvalStates(msgs)
	if st[1].part != 2 || st[1].of != 2 {
		t.Fatalf("card: %+v", st[1])
	}
	for _, id := range []uint64{2, 3, 4, 5} {
		if st[id].cont {
			t.Errorf("message %d taken as a part", id)
		}
	}
	msgs = append(msgs, mk(6, 1, "rocky", "#c1 (2/2)\n> the real one"))
	st = m.approvalStates(msgs)
	if st[1].part != 0 || !st[6].cont || st[6].part != 2 {
		t.Fatalf("card %+v part %+v", st[1], st[6])
	}
}

// wrapCells keeps every character, breaks at a space in a row's last
// third when there is one, and otherwise cuts at the width.
func TestWrapCells(t *testing.T) {
	for _, c := range []struct {
		s    string
		w    int
		want []string
	}{
		{"plan for /poll, about 15 min and ~$2:", 34, []string{"plan for /poll, about 15 min and ", "~$2:"}},
		{"0123456789abcdef0123456789", 10, []string{"0123456789", "abcdef0123", "456789"}},
		{"ab cdefghijklmnop", 10, []string{"ab cdefghi", "jklmnop"}},
		{"  two  spaces  kept  ", 8, []string{"  two  ", "spaces  ", "kept  "}},
		{"", 5, []string{""}},
	} {
		got := wrapCells(c.s, c.w)
		if strings.Join(got, "|") != strings.Join(c.want, "|") || strings.Join(got, "") != c.s {
			t.Errorf("wrapCells(%q, %d) = %q, want %q", c.s, c.w, got, c.want)
		}
		for _, p := range got {
			if strWidth(p) > c.w {
				t.Errorf("%q wider than %d", p, c.w)
			}
		}
	}
}

func TestAgentTitleRunes(t *testing.T) {
	for in, want := range map[string]string{"rocky": "Rocky", "élan": "Élan", "": "Your agent", "x": "X"} {
		if got := agentTitle(in); got != want {
			t.Errorf("agentTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

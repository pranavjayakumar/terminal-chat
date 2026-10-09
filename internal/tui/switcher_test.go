package tui

import "testing"

func TestFuzzyScore(t *testing.T) {
	if _, ok := fuzzyScore("xyz", "general"); ok {
		t.Fatal("xyz matched general")
	}
	if _, ok := fuzzyScore("bkd", "backend"); !ok {
		t.Fatal("bkd should match backend")
	}
	if _, ok := fuzzyScore("", "anything"); !ok {
		t.Fatal("empty pattern matches all")
	}
	if _, ok := fuzzyScore("GEN", "general"); !ok {
		t.Fatal("case-insensitive")
	}
	better := func(p, a, b string) {
		t.Helper()
		sa, oka := fuzzyScore(p, a)
		sb, okb := fuzzyScore(p, b)
		if !oka || !okb || sa <= sb {
			t.Errorf("%q: want %q (%d) > %q (%d)", p, a, sa, b, sb)
		}
	}
	better("gen", "general", "big-engine")
	better("back", "backend", "big-ack")
	better("ops", "ops", "operations")
	better("gen", "general", "general-engineering-notes") // shorter wins on equal matches
}

func TestSwitcherFlow(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	run(m, m.handleKey(keyMsg("ctrl+k")))
	if m.mode != modeSwitcher {
		t.Fatal("ctrl+k didn't open switcher")
	}
	// Bob has a DM, carol doesn't; random is public but not joined.
	labels := map[string]uint64{}
	for _, it := range m.sw.items {
		labels[it.label] = it.ch
	}
	if labels["@ bob"] != 20 {
		t.Fatalf("bob should map to the DM: %v", labels)
	}
	if ch, ok := labels["@ carol  Carol C"]; !ok || ch != 0 {
		t.Fatalf("carol should be a new DM: %v", labels)
	}
	for _, r := range "bkend" {
		run(m, m.handleKey(keyMsg(string(r))))
	}
	if len(m.sw.shown) == 0 || m.sw.shown[0].ch != 11 {
		t.Fatalf("top match: %+v", m.sw.shown)
	}
	run(m, m.handleKey(keyMsg("enter")))
	if m.mode != modeNormal || m.active != 11 {
		t.Fatalf("enter didn't open backend: mode %v active %d", m.mode, m.active)
	}
	// Opening a person without a DM creates one.
	run(m, m.handleKey(keyMsg("ctrl+k")))
	for _, r := range "carol" {
		run(m, m.handleKey(keyMsg(string(r))))
	}
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("dm") {
		t.Fatal("OpenDM not called")
	}
	// Opening a public channel we're not in joins it.
	run(m, m.handleKey(keyMsg("ctrl+k")))
	for _, r := range "random" {
		run(m, m.handleKey(keyMsg(string(r))))
	}
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("join") || m.active != 12 {
		t.Fatalf("join random: active %d calls %v", m.active, f.calls)
	}
	run(m, m.handleKey(keyMsg("ctrl+k")))
	run(m, m.handleKey(keyMsg("esc")))
	if m.mode != modeNormal {
		t.Fatal("esc didn't close switcher")
	}
}

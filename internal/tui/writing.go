package tui

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// Writing shortcuts (issue #132): quoting a message, quick reactions and
// ++, wrapping a pasted log in a code block, s/old/new on my last message,
// opening and listing links, and the sidebar's mute toggle.

// inputOffers holds the one-shot offers the input makes. Each is armed only
// while its hint is the notice on screen and the input still holds what it
// held when the hint appeared, so a stale offer can never act.
type inputOffers struct {
	hint string // the notice text of the live offer

	// A paste of 3+ lines: Tab wraps runes [wrapFrom, wrapTo) of wrapVal.
	wrapVal          string
	wrapFrom, wrapTo int
	// s/old/new waiting for a second Enter.
	subVal  string
	subID   uint64 // the message the hint previewed, and its text then
	subOrig string
	dk      draftKey // where the offer was made

	pasteVal string // the input right after the last paste

	// A dragged-in file path or an over-long paste (files.go): Tab sends
	// attachPaths, or attachText as paste.txt, and drops runes
	// [attachFrom, attachTo) of attachVal from the input.
	attachVal            string
	attachFrom, attachTo int
	attachPaths          []string
	attachText           string
}

// dropWriteHints ends any offer (the input changed or moved elsewhere).
func (m *Model) dropWriteHints() {
	w := &m.wr
	if w.hint != "" && m.notice == w.hint {
		m.clearNotice()
	}
	*w = inputOffers{}
}

// offerHint shows an offer's hint and remembers it as the live one.
func (m *Model) offerHint(s string) {
	m.setHint(s)
	m.wr.hint, m.wr.dk = m.notice, m.dk
}

// staleOffers takes down an offer's hint once the input no longer holds
// what it was made for (a picker insert, a completion, a quote).
func (m *Model) staleOffers() {
	w := &m.wr
	if w.hint == "" || m.notice != w.hint {
		return
	}
	if v := m.input.Value(); w.dk != m.dk || (w.wrapVal != "" && v != w.wrapVal) || (w.subVal != "" && v != w.subVal) || (w.attachVal != "" && v != w.attachVal) {
		m.dropWriteHints()
	}
}

func (m *Model) offerLive() bool {
	return m.wr.hint != "" && m.notice == m.wr.hint && m.wr.dk == m.dk
}

// ---- quote ----

// maxQuoteLines caps a quote: a reply quotes the gist, not a whole log.
const maxQuoteLines = 10

// quotable reports whether x has text to quote.
func quotable(x client.Message) bool {
	return !x.Deleted && x.Sec != client.SecWaiting && x.Sec != client.SecInvalid &&
		strings.TrimSpace(sanitize.Text(x.Body)) != ""
}

// quoteText turns a message body into "> " lines and a fresh line after
// them for the reply.
func quoteText(body string) string {
	lines := strings.Split(strings.Trim(body, "\n"), "\n")
	cut := len(lines) > maxQuoteLines
	if cut {
		lines = lines[:maxQuoteLines]
	}
	var b strings.Builder
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			b.WriteString(">\n")
			continue
		}
		b.WriteString("> " + l + "\n")
	}
	if cut {
		b.WriteString("> …\n")
	}
	return b.String()
}

// quoteSelected leaves select mode with the selected message quoted in the
// input, after whatever was there.
func (m *Model) quoteSelected(sel client.Message) {
	if !quotable(sel) {
		m.setInfo("nothing to quote in this message")
		return
	}
	q := quoteText(sanitize.Text(sel.Body))
	m.leaveSelect()
	cur := m.input.Value()
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	m.input.SetHeight(m.inputMax()) // see layoutInput
	m.input.SetValue(cur + q)
	m.hideComp()
	m.layoutInput()
}

// quoteDepth reads a rendered line's quote markers: "> text" is depth 1,
// "> > text" depth 2; a lone ">" is an empty quote line. Only "> " counts,
// so ">>> prompts", "->" and ">_<" stay text.
func quoteDepth(ln string) (int, string) {
	d := 0
	for {
		switch {
		case strings.HasPrefix(ln, "> "):
			ln = ln[2:]
		case ln == ">":
			ln = ""
		default:
			return d, ln
		}
		d++
	}
}

// ---- quick reactions ----

const maxRecentReacts = 5

// recentReacts is what the react prompt offers on 1–5: the reactions I
// added lately, newest first, topped up from the frequently used list.
func (m *Model) recentReacts() []string {
	out := slices.Clone(m.ui.Reacts)
	for _, e := range m.freqList() {
		if len(out) >= maxRecentReacts {
			break
		}
		if gridSafe(e) && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		out = freqSeed[:1] // never empty: ++ takes the first
	}
	return out[:min(len(out), maxRecentReacts)]
}

// noteReact puts e first in the recent reactions and saves them.
func (m *Model) noteReact(e string) {
	if !gridSafe(e) {
		return
	}
	list := slices.DeleteFunc(slices.Clone(m.ui.Reacts), func(s string) bool { return s == e })
	list = append([]string{e}, list...)
	m.ui.Reacts = list[:min(len(list), maxRecentReacts)]
	m.savePrefs()
}

// pickRecent reacts with recent reaction n (1-based) from the picker.
func (m *Model) pickRecent(n int) (tea.Cmd, bool) {
	rs := m.recentReacts()
	if !m.emo.react || m.emo.query != "" || n < 1 || n > min(len(rs), m.recentShown()) {
		return nil, false
	}
	e := rs[n-1]
	m.closePicker(true)
	m.noteEmoji(e)
	return m.reactWith(m.currentTarget(), e), true
}

// recentShown is how many recent reactions the picker's search row has
// room for (pickSearch draws them the same way), so a digit never picks one
// that isn't on screen.
func (m *Model) recentShown() int {
	e := &m.emo
	if !e.open || ease(m.progress(e.at, dToast)) < 0.4 {
		return 0
	}
	b := m.pickSize(m.geo())
	if b.cols < 1 {
		return 0
	}
	w := b.r.x1 - b.r.x0 // the search box: inside the frame's padding, or the whole width
	x := 0
	if b.framed {
		x, w = 2, w-4
	}
	tx, end, n := x+4, x+w-1, 0
	for range maxRecentReacts {
		if tx+4 > end {
			break
		}
		tx += 5
		n++
	}
	return n
}

// lastReactable is the newest message in p that takes a reaction.
func lastReactable(p *pane) (client.Message, bool) {
	for i := len(p.msgs) - 1; i >= 0; i-- {
		x := p.msgs[i]
		if x.ID != 0 && !x.Deleted && x.Failed == "" && x.Sec != client.SecWaiting && x.Sec != client.SecInvalid {
			return x, true
		}
	}
	return client.Message{}, false
}

// quickReact is "++": the last message gets my last reaction.
func (m *Model) quickReact() tea.Cmd {
	p := m.focus()
	if !p.open() {
		m.setInfo(m.kn.of("no channel open — ⌥K to pick one"))
		return nil
	}
	x, ok := lastReactable(p)
	if !ok {
		m.setInfo("nothing to react to here")
		return nil
	}
	e := m.recentReacts()[0]
	m.clearInput()
	if slices.Contains(x.Reactions[e], m.me.ID) {
		m.setInfo("you already reacted " + e)
		return nil
	}
	m.noteEmoji(e)
	m.setInfo("reacted " + e + " to " + m.atName(x.Author))
	return m.reactWith(x, e)
}

// ---- wrapping a pasted log in a code block ----

// offerWrap arms Tab after a bracketed paste of 3+ lines that isn't
// already code: before is the input's text before the paste.
func (m *Model) offerWrap(before string) {
	after := m.input.Value()
	if strings.HasPrefix(after, "/") && !strings.HasPrefix(after, "//") {
		return // a command's argument, not a message
	}
	n := utf8.RuneCountInString(after) - utf8.RuneCountInString(before)
	end := m.cursorOffset()
	start := end - n
	rs := []rune(after)
	if n <= 0 || start < 0 || end > len(rs) {
		return
	}
	ins := string(rs[start:end])
	pre := string(rs[:start])
	if strings.Count(strings.Trim(ins, "\n"), "\n") < 2 || strings.Contains(ins, "```") {
		return
	}
	if lines := strings.Split(pre, "\n"); inCode(pre, len(lines)-1, utf8.RuneCountInString(lines[len(lines)-1])) {
		return // pasted inside a block already
	}
	m.offerHint(m.kn.of("⇥ wraps the paste in a ``` code block"))
	m.wr.wrapVal, m.wr.wrapFrom, m.wr.wrapTo = after, start, end
}

// cursorOffset is the cursor's position in the input's text, in runes.
func (m *Model) cursorOffset() int {
	lines := strings.Split(m.input.Value(), "\n")
	row := min(m.input.Line(), len(lines)-1)
	off := 0
	for _, l := range lines[:max(row, 0)] {
		off += utf8.RuneCountInString(l) + 1
	}
	_, col := m.cursorLine()
	return off + col
}

func (m *Model) wrapArmed() bool {
	return m.offerLive() && m.wr.wrapVal != "" && m.input.Value() == m.wr.wrapVal
}

// wrapPaste puts the offered paste between ``` fences, each on its own line.
func (m *Model) wrapPaste() {
	rs := []rune(m.wr.wrapVal)
	pre, ins, post := string(rs[:m.wr.wrapFrom]), string(rs[m.wr.wrapFrom:m.wr.wrapTo]), string(rs[m.wr.wrapTo:])
	var b strings.Builder
	b.WriteString(pre)
	if pre != "" && !strings.HasSuffix(pre, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n" + strings.Trim(ins, "\n") + "\n```")
	if !strings.HasPrefix(post, "\n") {
		b.WriteString("\n") // the cursor lands below the fence, not on it
	}
	b.WriteString(post)
	m.dropWriteHints()
	m.input.SetHeight(m.inputMax()) // see layoutInput
	m.input.SetValue(b.String())
	m.layoutInput()
	m.setInfo("wrapped in a code block")
}

// ---- s/old/new ----

var substRe = regexp.MustCompile(`^s/([^/\n]+)/([^/\n]*)/?$`)

// parseSubst reads "s/old/new" or "s/old/new/".
func parseSubst(s string) (old, repl string, ok bool) {
	g := substRe.FindStringSubmatch(s)
	if g == nil {
		return "", "", false
	}
	return g[1], g[2], true
}

// lastOwn is my newest message in p that isn't deleted or failed.
func (m *Model) lastOwn(p *pane) (client.Message, bool) {
	for i := len(p.msgs) - 1; i >= 0; i-- {
		if x := p.msgs[i]; m.ownMsg(x) && !x.Deleted && x.Failed == "" {
			return x, true
		}
	}
	return client.Message{}, false
}

// trySubst handles "s/old/new" as the whole input: when old is in my last
// message here, the first Enter shows what will change and the second
// fixes it. Otherwise (or for a paste) it is an ordinary message.
func (m *Model) trySubst(v, body string) (tea.Cmd, bool) {
	old, repl, ok := parseSubst(body)
	if !ok || v == m.wr.pasteVal {
		return nil, false
	}
	p := m.focus()
	x, ok := m.lastOwn(p)
	if !ok || !x.Sec.Trusted() {
		return nil, false
	}
	orig := sanitize.Text(x.Body)
	if !strings.Contains(orig, old) {
		return nil, false
	}
	if x.ID == 0 {
		// Typed right after sending: the fix waits, it is never sent as text.
		m.setInfo("your last message is still sending — Enter again once it has")
		return nil, true
	}
	if !(m.offerLive() && m.wr.subVal == v && m.wr.subID == x.ID && m.wr.subOrig == orig) {
		m.dropWriteHints()
		m.offerHint(m.kn.of("⏎ again fixes your last message: ") + "“" + truncate(sanitize.Line(old), 16) + "” → “" +
			truncate(sanitize.Line(repl), 16) + "” · \\s/… sends it as text")
		m.wr.subVal, m.wr.subID, m.wr.subOrig = v, x.ID, orig
		return nil, true
	}
	m.dropWriteHints()
	body = strings.Replace(orig, old, repl, 1)
	changed := false
	body = expandShortcodes(body, orig, func(e string) { changed = m.countEmoji(e) || changed })
	if changed {
		m.savePrefs()
	}
	if strings.TrimSpace(body) == "" {
		m.setErr("", errors.New("that leaves your message empty — select it and d deletes it"))
		return nil, true
	}
	m.clearInput()
	id := x.ID
	return m.callThen("edit", func(ctx context.Context) (tea.Msg, error) {
		return infoMsg("fixed your last message"), m.c.Edit(ctx, id, body)
	}), true
}

// unescapeShortcut: "\++" and "\s/a/b" send as typed, without the "\".
func unescapeShortcut(body string) string {
	if rest, ok := strings.CutPrefix(body, `\`); ok {
		if _, _, sub := parseSubst(rest); rest == "++" || sub {
			return rest
		}
	}
	return body
}

// ---- links ----

const maxLinkLen = 2048

// safeURL reports whether s may be handed to the terminal (OSC 8) and to
// open/xdg-open: an http(s) URL with a host, printable ASCII only (no
// spaces, escapes or controls), no user:password@ part.
func safeURL(s string) bool {
	if len(s) > maxLinkLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" {
		return false
	}
	sch := strings.ToLower(u.Scheme)
	return sch == "http" || sch == "https"
}

// msgLinks lists the links a body shows as links (not those in code),
// once each, in order.
func msgLinks(body string) []string {
	var out []string
	for _, sg := range splitFences(strings.Split(sanitize.Text(body), "\n")) {
		if sg.code {
			continue
		}
		for _, ln := range sg.lines {
			for _, sp := range parseInline(ln, "") {
				if sp.f&fURL != 0 && safeURL(sp.text) && !slices.Contains(out, sp.text) {
					out = append(out, sp.text)
				}
			}
		}
	}
	return out
}

// linkOf is the URL a frame action names ("url:<pane>:<ref>:<url>"), or "".
func linkOf(a string) string {
	if !strings.HasPrefix(a, "url:") {
		return ""
	}
	parts := strings.SplitN(a, ":", 4)
	if len(parts) < 4 || !safeURL(parts[3]) {
		return ""
	}
	return parts[3]
}

// linkID is a short stable OSC 8 id for u.
func linkID(u string) string {
	h := fnv.New64a()
	h.Write([]byte(u))
	return strconv.FormatUint(h.Sum64(), 36)
}

// hyperlinks reports whether the terminal takes OSC 8 links. Only ones
// known to (unknown ones may print the sequence), never inside a
// multiplexer; TC_LINKS=on|off overrides.
func hyperlinks(env func(string) string) bool {
	switch strings.ToLower(env("TC_LINKS")) {
	case "on", "1", "yes":
		return true
	case "off", "0", "no":
		return false
	}
	if env("TMUX") != "" || env("STY") != "" || env("HERDR_PANE_ID") != "" || env("ZELLIJ") != "" {
		return false
	}
	switch env("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "ghostty", "kitty", "vscode":
		return true
	}
	switch env("TERM") {
	case "xterm-kitty", "xterm-ghostty", "wezterm", "foot", "foot-extra", "alacritty":
		return true
	}
	if env("KITTY_WINDOW_ID") != "" || env("WT_SESSION") != "" {
		return true
	}
	v, err := strconv.Atoi(env("VTE_VERSION"))
	return err == nil && v >= 5000
}

// openLink opens u in the browser; over SSH (no browser here) or without
// an opener it copies it instead.
func (m *Model) openLink(u string) tea.Cmd {
	if !safeURL(u) {
		m.setErr("", errors.New("only http and https links open"))
		return nil
	}
	open := m.opt.OpenURL
	if open == nil || m.opt.SSH || m.opt.NoBrowser {
		return m.copyCmd(u)
	}
	ctx := m.ctx
	return func() tea.Msg {
		if err := open(ctx, u); err != nil {
			return errMsg{"open link", err}
		}
		return infoMsg("opened " + truncate(u, 48))
	}
}

// defaultOpenURL hands u to the desktop's opener as one argument, with no
// shell in between.
func defaultOpenURL(_ context.Context, u string) error {
	if !safeURL(u) {
		return errors.New("only http and https links open")
	}
	name := "xdg-open"
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		return errors.New("opening links isn't supported here")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s not found", name)
	}
	cmd := exec.Command(path, u) // stdin/stdout/stderr: /dev/null, so nothing draws over the UI
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	// xdg-open may live as long as the browser it starts, so nothing waits
	// for it here; this goroutine only reaps it and ends when it exits.
	go func() { _ = cmd.Wait() }()
	return nil
}

// openSelectedLink is l in select mode: one link opens, several list.
func (m *Model) openSelectedLink(sel client.Message) tea.Cmd {
	var links []string
	if !sel.Deleted && sel.Sec != client.SecWaiting && sel.Sec != client.SecInvalid {
		links = msgLinks(sel.Body)
	}
	switch len(links) {
	case 0:
		m.setInfo("no link in this message")
		return nil
	case 1:
		return m.openLink(links[0])
	}
	m.list = listState{title: m.kn.of("Links in this message · ⏎ open · y copy"), rows: links, urls: links, back: modeSelect}
	m.mode = modeList
	return nil
}

// maxLinks caps /links; linksScan is how many recent messages it reads.
const (
	maxLinks  = 100
	linksScan = 500
)

// showLinks is /links: this channel's recent links, newest first.
func (m *Model) showLinks() error {
	if m.active == 0 {
		return errors.New("no channel open")
	}
	var rows, urls []string
	msgs := m.c.Messages(m.active, linksScan)
	for i := len(msgs) - 1; i >= 0 && len(urls) < maxLinks; i-- {
		x := msgs[i]
		if x.Deleted || x.Sec == client.SecWaiting || x.Sec == client.SecInvalid {
			continue
		}
		for _, u := range msgLinks(x.Body) {
			if slices.Contains(urls, u) || len(urls) >= maxLinks {
				continue
			}
			who := truncate(strings.TrimPrefix(m.atName(x.Author), "@"), 12)
			rows = append(rows, fit(who, 12)+"  "+u)
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		m.setInfo("no links in the recent messages here")
		return nil
	}
	m.list = listState{title: "Links in " + m.chanTitle(m.active) + m.kn.of(" · ⏎ open · y copy"), rows: rows, urls: urls}
	m.mode = modeList
	return nil
}

// ---- sidebar mute ----

// toggleMute mutes a channel from the sidebar's hover button or ⌥M (the
// open channel, or the palette's highlighted one), or puts it back to the
// default level (mentions, when the default is off).
func (m *Model) toggleMute(ch uint64) tea.Cmd {
	nb, ok := m.notifyBackend()
	if !ok {
		m.setErr("", errors.New("notification settings aren't available"))
		return nil
	}
	if _, ok := m.chanView(ch); !ok {
		return nil
	}
	muted := m.muted(ch)
	lvl := client.LevelOff
	if muted {
		lvl = client.LevelDefault
	}
	if err := nb.SetChanNotify(ch, lvl, 0); err != nil {
		m.setErr("mute", err)
		return nil
	}
	m.refreshPrefs()
	if muted && m.muted(ch) {
		// the default level is off: unmuting means mentions here
		if err := nb.SetChanNotify(ch, client.LevelMentions, 0); err != nil {
			m.setErr("mute", err)
			return nil
		}
		m.refreshPrefs()
	}
	if muted {
		m.setInfo("unmuted " + m.chanTitle(ch))
	} else {
		// Name only a key that acts on this channel once the palette closes.
		undo := "click unmute to undo"
		switch {
		case ch == m.active:
			undo = m.kn.of("⌥M") + " undoes"
		case m.mode == modeSwitcher:
			undo = m.kn.of("⌥M") + " on its row undoes"
		}
		m.setInfo("muted " + m.chanTitle(ch) + " · no popups or sounds · " + undo)
	}
	return nil
}

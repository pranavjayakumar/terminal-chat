package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

func newInput(st *styles) textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0 // no line cap; the visible height is capped in layoutInput
	ta.MaxWidth = 0
	ta.SetPromptFunc(2, func(i int) string {
		if i == 0 {
			return st.prompt.Render("> ")
		}
		return "  "
	})
	plain := textarea.Style{
		Base: st.base, CursorLine: st.base, CursorLineNumber: st.base, EndOfBuffer: st.base,
		LineNumber: st.base, Placeholder: st.placeholder, Prompt: st.base, Text: st.base,
	}
	ta.FocusedStyle, ta.BlurredStyle = plain, plain
	// Enter sends; these insert a newline.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys(kAltEnter, kCtrlJ))
	// Ctrl-K/Ctrl-N/Ctrl-P are ours.
	ta.KeyMap.DeleteAfterCursor = key.NewBinding(key.WithDisabled())
	ta.KeyMap.LineNext = key.NewBinding(key.WithKeys(kDown))
	ta.KeyMap.LinePrevious = key.NewBinding(key.WithKeys(kUp))
	ta.SetHeight(1)
	ta.Focus()
	return ta
}

func newLine(st *styles) textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.PromptStyle = st.prompt
	ti.TextStyle = st.base
	ti.PlaceholderStyle = st.placeholder
	ti.CharLimit = 200
	return ti
}

// inputLines is how many rows the textarea needs for v at text width w.
// It mirrors bubbles/textarea's soft wrap (which isn't exported), including
// the extra row it adds when a line exactly fills the width.
func inputLines(v string, w int) int {
	w = max(w, 1)
	n := 0
	for _, l := range strings.Split(v, "\n") {
		n += taWrapCount([]rune(l), w)
	}
	return n
}

func taWrapCount(runes []rune, width int) int {
	rows, lineW := 1, 0
	var word []rune
	spaces := 0
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			ww := uniseg.StringWidth(string(word))
			if lineW+ww+spaces > width {
				rows++
				lineW = 0
			}
			lineW += ww + spaces
			spaces, word = 0, nil
		} else if uniseg.StringWidth(string(word))+runewidth.RuneWidth(word[len(word)-1]) > width {
			if lineW > 0 {
				rows++
			}
			lineW = uniseg.StringWidth(string(word))
			word = nil
		}
	}
	if lineW+uniseg.StringWidth(string(word))+spaces >= width {
		rows++
	}
	return rows
}

// inputMax is the most rows the input may take.
func (m *Model) inputMax() int { return min(max(m.h/3, 1), maxInputH) }

// layoutInput sizes the textarea for the current layout. The textarea only
// scrolls its viewport when the cursor leaves it, so it is first grown to
// the maximum (scrolling back to the top when everything fits) and then
// shrunk to what the content needs.
func (m *Model) layoutInput() {
	d := m.dims()
	if d.mainW <= 2 {
		return
	}
	m.input.SetWidth(d.mainW)
	m.input.SetHeight(m.inputMax())
	m.input, _ = m.input.Update(nil)
	m.input.SetHeight(d.inputH)
	m.line.Width = max(d.mainW-strWidth(m.line.Prompt)-1, 1)
}

// selectFooter lists only the keys that do something for the selected
// message, so it never offers an edit of someone else's message.
func (m *Model) selectFooter() string {
	sel, ok := m.selected()
	if !ok {
		return "Esc leaves select mode"
	}
	var keys []string
	if sel.Failed != "" {
		keys = []string{"r retry", "d discard", "v view", "y copy"}
	} else {
		live := sel.ID != 0 && !sel.Deleted
		mine := sel.Author == m.me.ID
		if mine && live && sel.Sec.Trusted() {
			keys = append(keys, "e edit")
		}
		if mine && live {
			keys = append(keys, "d delete")
		}
		if live {
			keys = append(keys, "r thread", "+ react")
			if sel.Pinned {
				keys = append(keys, "p unpin")
			} else {
				keys = append(keys, "p pin")
			}
		}
		keys = append(keys, "v view", "y copy")
	}
	full := strings.Join(keys, " · ") + " · Esc"
	if strWidth(full) < m.dims().mainW-1 {
		return full
	}
	// Narrow: just the key letters, Esc last so leaving is always visible.
	letters := make([]string, len(keys))
	for i, k := range keys {
		letters[i], _, _ = strings.Cut(k, " ")
	}
	return strings.Join(letters, " ") + " · Esc"
}

func (m *Model) updatePlaceholder() {
	switch {
	case m.mode == modeSelect:
		m.input.Placeholder = m.selectFooter()
	case m.thread.open():
		m.input.Placeholder = "reply in thread"
	case m.active != 0:
		m.input.Placeholder = "message " + m.chanTitle(m.active)
	default:
		m.input.Placeholder = "Ctrl-K to open a channel"
	}
}

// submit handles Enter in the input: save an edit, run a command or send.
func (m *Model) submit() tea.Cmd {
	v := m.input.Value()
	if strings.TrimSpace(v) == "" {
		return nil
	}
	body := strings.TrimRight(v, " \t\n")
	if m.editing != 0 {
		id := m.editing
		m.cancelEdit()
		return m.call("edit", func(ctx context.Context) error { return m.c.Edit(ctx, id, body) })
	}
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		cmd, err := m.runCommand(v)
		if err != nil {
			m.setErr("", err) // input kept so it can be fixed
			return nil
		}
		m.clearInput()
		return cmd
	}
	if strings.HasPrefix(body, "//") {
		body = body[1:] // "//x" sends "/x"
	}
	p := m.focus()
	if !p.open() {
		m.setInfo("no channel open — Ctrl-K to pick one")
		return nil
	}
	if v, _ := m.chanView(p.ch); v.Removed {
		m.setErr("", errors.New("not sent: you were removed from this channel"))
		return nil
	}
	if st := m.e2e[p.ch]; st.State == client.E2EAlarm {
		// Input kept: nothing is sent while the channel's keys are suspect.
		m.setErr("", fmt.Errorf("not sent: this channel's encryption is in alarm (%s)", sanitize.Line(st.Detail)))
		return nil
	}
	if us := m.keyChg[p.ch]; len(us) > 0 {
		n := m.atName(us[0])
		m.setErr("", fmt.Errorf("not sent: /accept-key %s, then /verify %s — %s's key changed", n, n, n))
		return nil
	}
	if err := m.c.Send(p.ch, body, p.root); err != nil {
		m.setErr("send", err)
		return nil
	}
	m.clearInput()
	m.clearErr()
	p.scroll = 0
	m.reloadPane(p)
	return nil
}

func (m *Model) clearInput() {
	m.input.Reset()
	m.layoutInput()
}

// ---- slash commands ----

type command struct {
	name string
	args []string
	rest string // everything after the name, trimmed
}

// parseCommand splits "/name args..." into its parts.
func parseCommand(s string) (command, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") {
		return command{}, errors.New("not a command")
	}
	body := s[1:]
	name, rest := body, ""
	if i := strings.IndexFunc(body, unicode.IsSpace); i >= 0 {
		name, rest = body[:i], strings.TrimSpace(body[i:])
	}
	if name == "" {
		return command{}, errors.New("empty command")
	}
	return command{name: strings.ToLower(name), args: strings.Fields(rest), rest: rest}, nil
}

func (m *Model) findChan(name string) (uint64, bool) {
	name = strings.ToLower(strings.TrimPrefix(name, "#"))
	for _, v := range m.chans {
		if v.Kind != proto.ChanDM && v.Name == name {
			return v.ID, true
		}
	}
	return 0, false
}

func (m *Model) findUser(name string) (uint64, bool) {
	name = strings.ToLower(strings.TrimPrefix(name, "@"))
	for _, u := range m.userList {
		if strings.ToLower(u.Name) == name {
			return u.ID, true
		}
	}
	return 0, false
}

// runCommand executes a slash command. An error means nothing happened and
// the input should be kept.
func (m *Model) runCommand(s string) (tea.Cmd, error) {
	c, err := parseCommand(s)
	if err != nil {
		return nil, err
	}
	need := func(n int, usage string) error {
		if len(c.args) < n {
			return fmt.Errorf("usage: %s", usage)
		}
		return nil
	}
	needChan := func() (uint64, error) {
		if m.active == 0 {
			return 0, errors.New("no channel open")
		}
		return m.active, nil
	}
	be := m.c
	switch c.name {
	case "quit", "q":
		return tea.Quit, nil
	case "help":
		m.openHelp()
		return nil, nil
	case "activity":
		m.openActivity()
		return nil, nil
	case "join":
		if err := need(1, "/join #channel"); err != nil {
			return nil, err
		}
		id, ok := m.findChan(c.args[0])
		if !ok {
			return nil, fmt.Errorf("unknown channel %s", sanitize.Line(c.args[0]))
		}
		return m.openChannel(id), nil
	case "leave":
		ch, err := needChan()
		if err != nil {
			return nil, err
		}
		return m.callThen("leave", func(ctx context.Context) (tea.Msg, error) {
			return openChanMsg{}, be.Leave(ctx, ch)
		}), nil
	case "create":
		const usage = "/create #name [--private [--plaintext]]"
		if err := need(1, usage); err != nil {
			return nil, err
		}
		var name string
		private, plaintext := false, false
		for _, a := range c.args {
			switch {
			case a == "--private":
				private = true
			case a == "--plaintext":
				plaintext = true
			case name == "":
				name = strings.TrimPrefix(a, "#")
			default:
				return nil, errors.New("usage: " + usage)
			}
		}
		if plaintext && !private {
			return nil, errors.New("--plaintext goes with --private (public channels are never encrypted)")
		}
		if !proto.ValidName(name) {
			return nil, errors.New("channel names are 1-32 of a-z 0-9 _ - (lowercase)")
		}
		// Private channels are end-to-end encrypted unless asked otherwise
		// (--plaintext: e.g. for webhooks, which can't post into encrypted ones).
		if private && !plaintext {
			return m.callThen("create", func(ctx context.Context) (tea.Msg, error) {
				ch, err := be.CreateEncryptedChannel(ctx, name, "")
				if pe := (*proto.Error)(nil); errors.As(err, &pe) && pe.Code == proto.ErrBadRequest && strings.Contains(pe.Msg, "e2e1") {
					err = errors.New("this server can't encrypt channels; /create #" + name + " --private --plaintext makes an unencrypted one")
				}
				return openChanMsg{ch.ID}, err
			}), nil
		}
		return m.callThen("create", func(ctx context.Context) (tea.Msg, error) {
			ch, err := be.CreateChannel(ctx, name, private, "")
			return openChanMsg{ch.ID}, err
		}), nil
	case "add":
		if err := need(1, "/add @user"); err != nil {
			return nil, err
		}
		ch, err := needChan()
		if err != nil {
			return nil, err
		}
		var ids []uint64
		for _, a := range c.args {
			id, ok := m.findUser(a)
			if !ok {
				return nil, fmt.Errorf("unknown user %s", sanitize.Line(a))
			}
			ids = append(ids, id)
		}
		return m.call("add", func(ctx context.Context) error {
			for _, id := range ids {
				if err := be.AddMember(ctx, ch, id); err != nil {
					var ue *client.UnverifiedMembersError
					if errors.As(err, &ue) {
						// Names were sanitized by the client.
						return fmt.Errorf("not added: no verified device of %s — they can't get this channel's key yet", strings.Join(ue.Names, ", "))
					}
					return err
				}
			}
			return nil
		}), nil
	case "remove":
		if err := need(1, "/remove @user"); err != nil {
			return nil, err
		}
		ch, err := needChan()
		if err != nil {
			return nil, err
		}
		if v, _ := m.chanView(ch); v.Kind != proto.ChanPrivate {
			return nil, errors.New("/remove only works in private channels (/leave to go yourself)")
		}
		var ids []uint64
		for _, a := range c.args {
			id, ok := m.findUser(a)
			if !ok {
				return nil, fmt.Errorf("unknown user %s", sanitize.Line(a))
			}
			ids = append(ids, id)
		}
		return m.call("remove", func(ctx context.Context) error {
			for _, id := range ids {
				if err := be.RemoveMember(ctx, ch, id); err != nil {
					return err
				}
			}
			return nil
		}), nil
	case "encrypt":
		ch, err := needChan()
		if err != nil {
			return nil, err
		}
		v, _ := m.chanView(ch)
		switch {
		case v.E2E:
			return nil, errors.New("this channel is already end-to-end encrypted")
		case v.Kind == proto.ChanPublic:
			return nil, errors.New("public channels can't be encrypted")
		}
		return m.callThen("encrypt", func(ctx context.Context) (tea.Msg, error) {
			return infoMsg("🔒 now end-to-end encrypted — earlier messages are marked legacy"), be.EncryptChannel(ctx, ch)
		}), nil
	case "name":
		display := c.rest
		return m.call("name", func(ctx context.Context) error { return be.SetDisplay(ctx, display) }), nil
	case "topic":
		ch, err := needChan()
		if err != nil {
			return nil, err
		}
		topic := c.rest
		return m.call("topic", func(ctx context.Context) error { return be.SetTopic(ctx, ch, topic) }), nil
	case "dm":
		if err := need(1, "/dm @user [@user...]"); err != nil {
			return nil, err
		}
		var ids []uint64
		for _, a := range c.args {
			id, ok := m.findUser(a)
			if !ok && strings.EqualFold(strings.TrimPrefix(a, "@"), "me") {
				id, ok = m.me.ID, true // "@me" unless someone is called me
			}
			if !ok {
				return nil, fmt.Errorf("unknown user %s", sanitize.Line(a))
			}
			if id != m.me.ID {
				ids = append(ids, id)
			}
		}
		// Only me: my notes, where reminders land (a DM with nobody else).
		return m.openDM(ids), nil
	case "invite":
		return m.inviteCommand(c.args)
	case "away":
		status := c.rest
		m.manualAway, m.autoAway = true, false
		return m.call("away", func(context.Context) error { return be.SetAway(true, status) }), nil
	case "back":
		m.manualAway, m.autoAway = false, false
		return tea.Batch(m.call("back", func(context.Context) error { return be.SetAway(false, "") }), m.awayTick()), nil
	case "snippet":
		return m.shareSnippet(c.args)
	case "share-pane":
		if len(c.args) > 1 {
			return nil, errors.New("usage: /share-pane [pane id or label]")
		}
		return m.sharePane(c.rest)
	case "pins":
		ch, err := needChan()
		if err != nil {
			return nil, err
		}
		return m.showPins(ch), nil
	case "search":
		if c.rest == "" {
			return nil, errors.New("usage: /search terms")
		}
		q := c.rest
		ctx := m.ctx
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, reqTimeout)
			defer cancel()
			res, err := be.Search(ctx, q, 0)
			return searchMsg{q, res, err}
		}, nil
	}
	if cmd, ok, err := m.trustCommand(c); ok {
		return cmd, err
	}
	if ok, err := m.highlightCommand(c); ok {
		return nil, err
	}
	if cmd, ok, err := m.hookCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.inviteManageCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.groupCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.remindCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.huddleCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.notifyCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.devicesCommand(c); ok {
		return cmd, err
	}
	if cmd, ok, err := m.disableCommand(c); ok {
		return cmd, err
	}
	return nil, fmt.Errorf("unknown command /%s", sanitize.Line(c.name))
}

// callThen runs a request in a Cmd and delivers its message on success.
func (m *Model) callThen(what string, f func(ctx context.Context) (tea.Msg, error)) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		msg, err := f(ctx)
		if err != nil {
			return errMsg{what, err}
		}
		return msg
	}
}

func (m *Model) openDM(users []uint64) tea.Cmd {
	be := m.c
	return m.callThen("dm", func(ctx context.Context) (tea.Msg, error) {
		ch, err := be.OpenDM(ctx, users)
		return openChanMsg{ch.ID}, err
	})
}

// ---- reactions ----

var emojiNames = map[string]string{
	"+1": "👍", "thumbsup": "👍", "-1": "👎", "thumbsdown": "👎",
	"tada": "🎉", "heart": "❤️", "smile": "😄", "joy": "😂", "laughing": "😆",
	"eyes": "👀", "rocket": "🚀", "fire": "🔥", "check": "✅", "white_check_mark": "✅",
	"thinking": "🤔", "pray": "🙏", "100": "💯", "ok": "👌", "ok_hand": "👌",
	"wave": "👋", "clap": "👏", "sob": "😭", "x": "❌", "warning": "⚠️",
}

// resolveEmoji turns "+1", ":tada:" or a literal emoji into the emoji to
// send.
func resolveEmoji(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if e, ok := emojiNames[strings.ToLower(strings.Trim(s, ":"))]; ok {
		return e, true
	}
	if s == "" || strings.ContainsAny(s, " \t\n") || sanitize.Line(s) != s || strWidth(s) > 4 || len(s) > 32 {
		return "", false
	}
	for _, r := range s {
		if r < 0x80 {
			return "", false
		}
	}
	return s, true
}

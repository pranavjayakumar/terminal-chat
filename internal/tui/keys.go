package tui

import tea "github.com/charmbracelet/bubbletea"

// Key names as Bubble Tea v1 spells them (tea.KeyMsg.String()).
const (
	kQuit       = "ctrl+c"
	kSwitcher   = "ctrl+k"
	kSidebar    = "ctrl+b"
	kEnter      = "enter"
	kEsc        = "esc"
	kUp         = "up"
	kDown       = "down"
	kPgUp       = "pgup"
	kPgDown     = "pgdown"
	kHelp       = "?"
	kHelpAlt    = "f1"
	kSidebarAlt = "f2" // Ctrl-B is tmux's prefix
	kHome       = "home"
	kEnd        = "end"
	kCtrlEnd    = "ctrl+end"
	kClearLine  = "ctrl+u"
	kAltEnter   = "alt+enter"
	kCtrlJ      = "ctrl+j"
	kTab        = "tab"
	kShiftTab   = "shift+tab"
	kSelEdit    = "e"
	kSelDelete  = "d"
	kSelReply   = "r"
	kSelReact   = "+"
	kSelCopy    = "y"
	kConfirmYes = "y"
)

// minSidebarCols is the narrowest terminal the sidebar can be forced into.
const minSidebarCols = 40

func isPrevChan(k string) bool   { return k == "alt+up" || k == "ctrl+p" }
func isNextChan(k string) bool   { return k == "alt+down" || k == "ctrl+n" }
func isPrevUnread(k string) bool { return k == "alt+shift+up" }
func isNextUnread(k string) bool { return k == "alt+shift+down" }

// helpLines is the help overlay, most important first.
var helpLines = [][2]string{
	{"Esc", "close / cancel; twice clears the draft"},
	{"Enter", "send"},
	{"Alt-Enter / Ctrl-J", "newline"},
	{"Ctrl-K", "quick switcher"},
	{"Alt-↑/↓, Ctrl-P/N", "previous / next channel"},
	{"Alt-Shift-↑/↓", "previous / next unread"},
	{"PgUp/PgDn, wheel", "scroll history"},
	{"End, Ctrl-End", "jump to newest"},
	{"↑ (empty input)", "select messages"},
	{"  e  d  r", "edit, delete, reply in thread"},
	{"  +  y", "react, copy"},
	{"  PgUp/PgDn  Home/End", "move by page / to ends"},
	{"  r / d on failed", "retry / discard"},
	{"Ctrl-U", "clear draft"},
	{"Ctrl-B, F2", "toggle sidebar"},
	{"F1, /help", "this help"},
	{"Ctrl-C twice, /quit", "quit"},
	{"", ""},
	{"/join #x", "join or open a channel"},
	{"/leave", "leave this channel"},
	{"/create #x [--private]", "new channel"},
	{"/add @user", "add someone here"},
	{"/topic text", "set the topic"},
	{"/name Display Name", "set your display name"},
	{"/dm @a @b", "direct message"},
	{"/search terms", "search; Enter jumps"},
	{"/highlight word…", "mark messages with these words like a mention (none: list)"},
	{"/unhighlight word…", "stop highlighting"},
	{"/away [status], /back", "presence"},
	{"/invite", "invite code (admin)"},
	{"/share-pane [pane]", "send a herdr pane's output (preview first)"},
	{"/webhook name", "webhook here; copies curl"},
	{"/webhooks", "list webhooks"},
	{"/webhook-delete id", "delete a webhook"},
	{"//text", "send text starting with /"},
}

// keyName is the key's name for matching against the constants above.
// Bubble Tea delivers fast typing, IME input and pastes as one KeyRunes
// message whose String() is the text itself, so typing "down" or "end"
// would otherwise act as the ↓ or End key. Only a single rune can name a key.
func keyName(k tea.KeyMsg) string {
	if k.Type == tea.KeyRunes && (len(k.Runes) != 1 || k.Paste) {
		return ""
	}
	return k.String()
}

package tui

import tea "github.com/charmbracelet/bubbletea"

// Key names as Bubble Tea v1 spells them (tea.KeyMsg.String()).
const (
	kQuit       = "ctrl+c"
	kSwitcher   = "ctrl+k"
	kActivity   = "alt+a" // Ctrl-A stays the textarea's line start
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
	kSelPin     = "p"
	kSelView    = "v"
	kConfirmYes = "y"
	kTalk       = "ctrl+t" // only inside a voice huddle (huddle.go)
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
	{"Alt-A, /activity", "mentions, replies, reactions, DMs"},
	{"Alt-↑/↓, Ctrl-P/N", "previous / next channel"},
	{"Alt-Shift-↑/↓", "previous / next unread"},
	{"PgUp/PgDn, wheel", "scroll history"},
	{"End, Ctrl-End", "jump to newest"},
	{"↑ (empty input)", "select messages"},
	{"  e  d  r", "edit, delete, reply in thread"},
	{"  +  y  v  p", "react, copy, view full message, pin / unpin"},
	{"  PgUp/PgDn  Home/End", "move by page / to ends"},
	{"  r / d on failed", "retry / discard"},
	{"Ctrl-U", "clear draft"},
	{"Ctrl-B, F2", "toggle sidebar"},
	{"F1, /help", "this help"},
	{"Ctrl-C twice, /quit", "quit"},
	{"", ""},
	{"/join #x", "join or open a channel"},
	{"/leave", "leave this channel"},
	{"/create #x [--private]", "new channel (private: end-to-end encrypted)"},
	{"  --private --plaintext", "private, not encrypted (for webhooks)"},
	{"/add @user", "add someone here"},
	{"/remove @user", "remove someone (private; creator or admin)"},
	{"/encrypt", "end-to-end encrypt this private channel or DM"},
	{"/verify @user", "compare safety numbers; v marks them verified"},
	{"/verify @user --github login", "check their key against github.com/login.keys"},
	{"/accept-key @user [fp]", "review and accept a changed key (unblocks sending)"},
	{"/members", "who's here; ? = not verified, ! = key changed"},
	{"/devices [@user]", "your (or someone's) devices; d revokes a lost one"},
	{"/device-revoke id", "revoke one of your devices (asks first)"},
	{"/disable @user", "admin: lock an account out for good (asks)"},
	{"/alarm", "why this channel's encryption is in alarm"},
	{"/alarm-clear #name", "clear it (type the name to confirm; audited)"},
	{"/topic text", "set the topic"},
	{"/name Display Name", "set your display name"},
	{"/dm @a @b", "direct message"},
	{"/search terms", "search; Enter jumps"},
	{"/pins", "pinned messages here; Enter jumps"},
	{"/highlight word…", "mark messages with these words like a mention (none: list)"},
	{"/unhighlight word…", "stop highlighting"},
	{"/away [status], /back", "presence"},
	{"/invite [uses] [ttl]", "invite: copies the install one-liner (e.g. /invite 5 3d)"},
	{"/invite --key github:user", "invite only someone holding that GitHub account's SSH keys"},
	{"/invites", "open invites with ids (yours; admins: all)"},
	{"/invite-revoke id", "make an invite unusable at once"},
	{"/share-pane [pane]", "send a herdr pane's output (preview first)"},
	{"/group name @a @b", "user group: @name mentions them all (admin; no users deletes)"},
	{"/groups", "list the groups"},
	{"/remind 10m text", "remind me in my notes (2h, 1d, 15:30, 9am, tomorrow 9am)"},
	{"/reminders", "pending reminders with ids"},
	{"/remind-delete id", "cancel a reminder"},
	{"/snippet file[:range]", "send file lines as a code block (preview first)"},
	{"/webhook name", "webhook here; copies curl"},
	{"/webhooks", "list webhooks"},
	{"/webhook-delete id", "delete a webhook"},
	{"/huddle", "join this channel's voice huddle (voice build)"},
	{"  Ctrl-T", "in a huddle: hold to talk, tap to keep the mic open"},
	{"/talk, /mute", "in a huddle: open / close the mic (when Ctrl-T doesn't work); Esc closes an open mic"},
	{"/leave-huddle", "leave the huddle"},
	{"/notify all|mentions|off [#chan] [for 2h]", "this channel's notifications (off = muted, dimmed)"},
	{"/notify default · global · content · quiet · test", "reset · default level · popup privacy · quiet hours · try it"},
	{"/dnd [2h|until 09:00|off]", "do not disturb: no popups, sounds or rings"},
	{"/sounds [on|off|<cue> on|off|volume n]", "notification and huddle sounds"},
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

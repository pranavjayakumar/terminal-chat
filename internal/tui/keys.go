package tui

import tea "github.com/charmbracelet/bubbletea"

// Key names as Bubble Tea v1 spells them (tea.KeyMsg.String()).
const (
	kQuit       = "ctrl+c"
	kSwitcher   = "alt+k" // the palette; Ctrl-K still works where it reaches us
	kActivity   = "alt+a" // Ctrl-A stays the textarea's line start
	kEmoji      = "alt+e" // the emoji picker
	kSidebar    = "ctrl+b"
	kEnter      = "enter"
	kEsc        = "esc"
	kUp         = "up"
	kLeft       = "left"
	kRight      = "right"
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
	kSelMenu    = "m" // the message menu
	kConfirmYes = "y"
	kTalk       = "ctrl+t" // only inside a voice huddle (huddle.go)
	kSettings   = "ctrl+," // only where the terminal reports it (kitty keys); /settings works everywhere
	kThrWider   = "alt+["  // the thread pane's edge moves left
	kThrNarrow  = "alt+]"
	kApproval   = "alt+o" // jump to my agent's waiting approval card (approval.go)
	kSelUnfold  = "o"     // unfold an agent run's steps (agentlive.go)
)

// minSidebarCols is the narrowest terminal the sidebar can be forced into.
const minSidebarCols = 40

func isPrevChan(k string) bool   { return k == "alt+up" || k == "ctrl+p" }
func isNextChan(k string) bool   { return k == "alt+down" || k == "ctrl+n" }
func isPrevUnread(k string) bool { return k == "alt+shift+up" }
func isNextUnread(k string) bool { return k == "alt+shift+down" }

// keyName is the key's name for matching against the constants above.
// Bubble Tea delivers fast typing, IME input and pastes as one KeyRunes
// message whose String() is the text itself, so typing "down" or "end"
// would otherwise act as the ↓ or End key. Only a single rune can name a key.
func keyName(k tea.KeyMsg) string {
	if k.Type == tea.KeyRunes && (len(k.Runes) != 1 || k.Paste) {
		return ""
	}
	if k.Type == tea.KeyRunes && !k.Alt {
		// macOS Option keys when Option types characters
		switch k.Runes[0] {
		case '˚':
			return kSwitcher
		case '“':
			return kThrWider
		case '‘':
			return kThrNarrow
		}
	}
	return k.String()
}

// isSwitcher: the key opens or closes the palette. Ctrl-K is a silent
// fallback: some terminals and multiplexers keep it for themselves.
func isSwitcher(key string) bool { return key == kSwitcher || key == "ctrl+k" }

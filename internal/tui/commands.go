package tui

// cmdSpec is one row of commandTable, the single list behind both the /help
// overlay and the / command menu, so the two can't drift apart.
type cmdSpec struct {
	name string // without the "/"; "" = a /help-only row (a note, not a command)
	args string // argument hint, e.g. "@user"
	desc string
	// help is the /help key column when it isn't "/name args"; helpHidden
	// keeps the command out of /help (it is covered by another row).
	help       string
	helpHidden bool
	more       bool // a further /help row for a command already listed: not in the menu
	admin      bool // only admins can run it: hidden from others' menus
	voice      bool // only useful in a huddle: hidden from the menu in builds without voice
}

// commandTable lists every slash command in /help order.
var commandTable = []cmdSpec{
	{name: "join", args: "#x", desc: "join or open a channel"},
	{name: "leave", desc: "leave this channel"},
	{name: "create", args: "#x [--private]", desc: "new channel (private: end-to-end encrypted)"},
	{help: "  --private --plaintext", desc: "private, not encrypted (for webhooks)"},
	{name: "add", args: "@user", desc: "add someone here"},
	{name: "remove", args: "@user", desc: "remove someone (private; creator or admin)"},
	{name: "encrypt", desc: "end-to-end encrypt this private channel or DM"},
	{name: "verify", args: "@user", desc: "compare safety numbers; v marks them verified"},
	{name: "verify", args: "@user --github login", desc: "check their key against github.com/login.keys", more: true},
	{name: "accept-key", args: "@user [fp]", desc: "review and accept a changed key (unblocks sending)"},
	{name: "members", desc: "who's here; ? = not verified, ! = key changed"},
	{name: "devices", args: "[@user]", desc: "your (or someone's) devices; d revokes a lost one"},
	{name: "device-revoke", args: "id", desc: "revoke one of your devices (asks first)"},
	{name: "disable", args: "@user", desc: "admin: lock an account out for good (asks)", admin: true},
	{name: "alarm", desc: "why this channel's encryption is in alarm"},
	{name: "alarm-clear", args: "#name", desc: "clear it (type the name to confirm; audited)"},
	{name: "topic", args: "text", desc: "set the topic (--clear removes it; none: show it)"},
	{name: "name", args: "Display Name", desc: "set your display name (--clear removes it; none: show it)"},
	{name: "dm", args: "@a @b", desc: "direct message (@me: your notes, where reminders land)"},
	{name: "search", args: "terms", desc: "search; Enter jumps"},
	{name: "pins", desc: "pinned messages here; Enter jumps"},
	{name: "highlight", args: "word…", desc: "mark messages with these words like a mention (none: list)"},
	{name: "unhighlight", args: "word…", desc: "stop highlighting"},
	{name: "away", args: "[status]", desc: "presence", help: "/away [status], /back"},
	{name: "back", desc: "presence: back from away", helpHidden: true},
	{name: "invite", args: "[uses] [ttl]", desc: "invite: copies the install one-liner (e.g. /invite 5 3d)"},
	{name: "invite", args: "--key github:user", desc: "invite only someone holding that GitHub account's SSH keys", more: true},
	{name: "invites", desc: "open invites with ids (yours; admins: all)"},
	{name: "invite-revoke", args: "id", desc: "make an invite unusable at once"},
	{name: "share-pane", args: "[pane]", desc: "send a herdr pane's output (preview first)"},
	{name: "group", args: "name @a @b", desc: "user group: @name mentions them all (admin; no users deletes)", admin: true},
	{name: "groups", desc: "list the groups"},
	{name: "remind", args: "10m text", desc: "remind me in my notes (2h, 1d, 15:30, 9am, tomorrow 9am)"},
	{name: "reminders", desc: "pending reminders with ids"},
	{name: "remind-delete", args: "id", desc: "cancel a reminder"},
	{name: "snippet", args: "file[:range]", desc: "send file lines as a code block (preview first)"},
	{name: "webhook", args: "name", desc: "webhook here; copies curl"},
	{name: "webhooks", desc: "list webhooks"},
	{name: "webhook-delete", args: "id", desc: "delete a webhook"},
	{name: "huddle", desc: "join this channel's voice huddle (voice build) · /huddle stats"},
	{name: "huddle", args: "stats", desc: "in a huddle: call quality (to report problems)", more: true, voice: true},
	{help: "  ⌃T", desc: "in a huddle: hold to talk, tap to keep the mic open"},
	{name: "talk", desc: "in a huddle: open / close the mic (when push-to-talk doesn't work); Esc closes an open mic", help: "/talk, /mute", voice: true},
	{name: "mute", desc: "in a huddle: close the mic", helpHidden: true, voice: true},
	{name: "leave-huddle", desc: "leave the huddle", voice: true},
	{name: "notify", args: "LEVEL [#ch] [for 2h]", desc: "all · mentions · off (muted, dimmed) · default"},
	{name: "notify", desc: "settings; also: global LEVEL, content full|name|none, quiet 22:00-08:00, test", more: true},
	{name: "dnd", args: "[2h|until 9am|off]", desc: "do not disturb: no popups, sounds or rings"},
	{name: "sounds", args: "[CUE] on|off", desc: "cues: mention message ring huddle join mic; /sounds volume N"},
	{name: "settings", desc: "theme, density, sidebar, notifications, devices"},
	{name: "theme", args: "[name]", desc: "switch theme (none: list them)"},
	{help: "//text", desc: "send text starting with /"},
	// In the keys part of /help ("Alt-A, /activity" and so on).
	{name: "activity", desc: "mentions, replies, reactions, DMs", helpHidden: true},
	{name: "help", desc: "keys and commands", helpHidden: true},
	{name: "quit", desc: "quit tc", helpHidden: true},
}

// helpKey is the row's /help key column.
func (c cmdSpec) helpKey() string {
	switch {
	case c.help != "":
		return c.help
	case c.args != "":
		return "/" + c.name + " " + c.args
	}
	return "/" + c.name
}

// isCommandName reports whether name (without "/") is a command runCommand
// knows, aliases included.
func isCommandName(name string) bool {
	if name == "q" {
		return true
	}
	for _, c := range commandTable {
		if c.name == name {
			return true
		}
	}
	return false
}

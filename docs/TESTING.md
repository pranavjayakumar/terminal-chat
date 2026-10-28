# Hands-on test checklist

For a person trying tc end to end. Automated tests (`make test`, chaos, malicious-server suite,
fuzzing) run in CI; this list covers what only a human at a real terminal can judge.

## Setup

1. Install and join with an invite (admin: the one `tc server` printed):
   `curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- --voice 'tc://…'`
   (drop `--voice` for the plain build). Then run `tc`; you land in `#general`.
2. A second user on the same machine, with a throwaway key:
   ```sh
   ssh-keygen -t ed25519 -N '' -f /tmp/bob_key
   TC_HOME=/tmp/tc-bob SSH_AUTH_SOCK= tc join 'tc://…team invite…' --key /tmp/bob_key --name bob
   TC_HOME=/tmp/tc-bob SSH_AUTH_SOCK= tc
   ```
   Clean up afterwards with `tc users disable @bob` (admin) and `rm -rf /tmp/tc-bob /tmp/bob_key*`.

## Features

| Area | Try | Expect |
|---|---|---|
| Chat | send, `Alt-Enter` newline, `↑` then `e`/`d`/`r`/`+`/`p`/`v` | edits, deletes, thread, reaction, pin, full view show on both sides |
| Navigation | `Ctrl-K`, `Alt-↑/↓`, `Alt-A`, `F1` | jump anywhere; activity lists mentions/replies/DMs |
| Encrypted DM | `/dm @bob`, send | 🔒 header; `/verify @bob`, compare numbers, `v` |
| Private channel | `/create #x --private`, `/add @bob` | 🔒, bob reads history; `/members` |
| Offline | kill the network or the server, send, reconnect | message sends once, in order |
| Shell | `echo hi \| tc send '#general'`, `tc tail '#general'` | code block; live stream |
| Invites | `/invite 2 1d`, `/invites`, `/invite-revoke id`; `tc invite --key github:<login>`; `tc invite --for @you` on a computer without your key | one-liner works; key-bound invite only works with that GitHub key; add-key invite signs the new computer in as you |
| Second computer | `tc login` on machine 1, run the printed line on machine 2 | same account, history, encrypted DMs readable |
| Devices | `/devices`, `/device-revoke id` | revoked device can't read new encrypted messages |
| Admin | `tc users`, `/disable @bob` | bob is locked out |
| Reminders | `/remind 1m test` | `⏰ test` in your notes after a minute, even if tc was closed |
| Update | `tc update --check` | reports current/newest |

## Notifications and sounds (v0.4.0)

Run `/notify test` first: it plays one popup per sound through the real backend.

| Where | Check |
|---|---|
| herdr | with `[ui.toast] delivery = "terminal"` and `"system"`: popups + sound; with `"off"` (herdr's default): nothing, as documented |
| macOS Terminal.app / iTerm2, outside herdr | system popup with sound; first run may ask for notification permission; Focus mode silences it |
| Linux GNOME / dunst | notify-send popup and sound |
| Over SSH: iTerm2, kitty, Ghostty, WezTerm, foot | popup appears on the local machine |
| tmux over SSH | works with `set -g allow-passthrough on`, falls back to the bell without |
| Levels | `/notify off for 5m` dims the channel, @mention still marks it; `/notify all` notifies every message; `/dnd 10m` silences everything; `/notify content name` hides text |

## v0.4.0 release test (two machines, voice build, real internet)

Both people install with `--voice` and join the production server.

1. `/huddle` in `#general`: both hear each other. On macOS the first time asks for mic permission.
2. Push-to-talk: hold `ctrl+t` while speaking, release, the mic closes within ~¼ s. Tap `ctrl+t`:
   mic stays open (red `●`) until the next tap or `Esc`.
3. Latency: count out loud together; mouth-to-ear should feel under ~200 ms.
4. Echo: one side on laptop speakers; the other shouldn't hear themselves back badly.
5. Cues: join/leave and mic on/off sounds; `/sounds volume 30` makes them quieter.
6. DM ring: `/dm @other`, `/huddle` there; the other side gets a ring popup + `📞` line, `Esc`
   dismisses it, `/huddle` joins.
7. Encrypted huddle: in that DM, revoke one of your other devices (`/devices`) during the call;
   audio continues after the key rotation.
8. Sidebar shows `🎧n` on the huddle channel for someone not in it; the herdr pane title shows `tc · 🎧`.
9. `/leave-huddle` on both sides; the `🎧` marker clears.

Report anything off as a GitHub issue (what you did, what you saw, both OSes and terminals).

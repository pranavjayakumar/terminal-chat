package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/voice"
)

var version = "dev"

const usage = `tc — terminal team chat

  tc                         open chat
  tc join <invite> [--name you] [--key ~/.ssh/id_ed25519]   (or the invite in $TC_INVITE)
                             join a team (--key may be a .pub held in ssh-agent)
  tc login tc://host:port#pin [--key path]
                             sign in on another computer with the same SSH key
  tc login                   show this computer's tc://host:port#pin
  tc send <#chan|@user> [message...]   (reads stdin if no message)
  tc send <#chan|@user> --file path[:start[-end]] [--context N] [message...]
  tc tail <#chan>            stream a channel
  tc notify [--to #chan|@user|@me] [--on-fail] -- <cmd...>
                             run a command, then post ✓/✗, time and output tail
  tc verify @user [--github login]
                             compare safety numbers (or GitHub keys) to verify someone
  tc devices [@user]         list your (or someone's) devices
  tc devices revoke <id> [--yes] [--self [--force]]
                             revoke a lost or stolen device
  tc invite [--uses N] [--ttl 72h] [--key github:user] [--raw]   (or --list, --revoke ID)
                             invite someone (admins; members if the server allows)
  tc users                   list everyone (admins also see who invited whom)
  tc users disable @user [--yes]
                             (admin) shut an account out for good, e.g. after a stolen SSH key
  tc server [--listen :7443] [--public host:port] [--data dir] [--member-invites=false]
  tc server invite [--uses N] [--ttl 72h] [--admin] [--key github:user] [--raw]
  tc update [--check]        install the latest release (or just check)
  tc version
`

func main() {
	server.Version = "tc/" + version
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "server":
		err = cmdServer(args)
	case "update":
		err = cmdUpdate(args)
	case "version", "--version", "-v":
		if voice.Available() {
			fmt.Println("tc", version, "(voice)")
		} else {
			fmt.Println("tc", version)
		}
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = runClient(cmd, args)
	}
	if errors.Is(err, errUsage) {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tc:", errorText(err))
		os.Exit(1)
	}
}

// errorText renders err for the terminal. Errors can carry server-supplied
// text, so each line is sanitized; multi-line explanations keep their lines.
func errorText(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for i, l := range lines {
		lines[i] = sanitize.Line(l)
	}
	return strings.Join(lines, "\n")
}

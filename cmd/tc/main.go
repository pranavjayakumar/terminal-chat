package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/server"
)

var version = "dev"

const usage = `tc — terminal team chat

  tc                         open chat
  tc login <invite> [--name you] [--key ~/.ssh/id_ed25519]
  tc send <#chan|@user> [message...]   (reads stdin if no message)
  tc send <#chan|@user> --file path[:start[-end]] [--context N] [message...]
  tc tail <#chan>            stream a channel
  tc notify [--to #chan|@user|@me] [--on-fail] -- <cmd...>
                             run a command, then post ✓/✗, time and output tail
  tc verify @user [--github login]
                             compare safety numbers (or GitHub keys) to verify someone
  tc server [--listen :7443] [--public host:port] [--data dir]
  tc server invite [--uses N] [--ttl 72h] [--admin] [--raw]
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
		fmt.Println("tc", version)
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
		// Errors can carry server-supplied text.
		fmt.Fprintln(os.Stderr, "tc:", sanitize.Line(err.Error()))
		os.Exit(1)
	}
}

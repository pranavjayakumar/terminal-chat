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
  tc tail <#chan>            stream a channel
  tc server [--listen :7443] [--public host:port] [--data dir]
  tc server invite [--admin]
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

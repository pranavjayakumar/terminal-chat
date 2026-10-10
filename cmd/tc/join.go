package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// joinEnv is everything the join wizard touches, so tests can fake it.
type joinEnv struct {
	in      io.Reader
	out     io.Writer
	tty     bool   // in is a terminal: prompts allowed
	home    string // TC_HOME
	sshDir  string // ~/.ssh
	gitName func() string
	user    string // $USER
	herdr   bool
	login   func(ctx context.Context, home, invite, name, keyPath string) (proto.User, error)
}

func osJoinEnv() joinEnv {
	sshDir := ""
	if h, err := os.UserHomeDir(); err == nil {
		sshDir = filepath.Join(h, ".ssh")
	}
	u := os.Getenv("USER")
	if u == "" {
		if cu, err := user.Current(); err == nil {
			u = cu.Username
		}
	}
	return joinEnv{
		in: os.Stdin, out: os.Stdout, tty: isTerminal(os.Stdin),
		home: client.DefaultHome(), sshDir: sshDir, gitName: gitUserName, user: u,
		herdr: os.Getenv("HERDR_ENV") == "1", login: client.Login,
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func gitUserName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "git", "config", "--get", "user.name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// cmdJoin: tc join [<invite>] [--name you] [--key path] (also `tc login`).
// Without an argument the invite comes from $TC_INVITE, which (unlike argv)
// other local users can't read in ps; install.sh passes it that way.
func cmdJoin(args []string) error {
	invite, name, key, err := parseJoinArgs(args, os.Getenv(inviteEnv))
	// Not passed on to anything tc runs (git, ssh-agent).
	os.Unsetenv(inviteEnv)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	return runJoin(ctx, osJoinEnv(), invite, name, key)
}

// inviteEnv holds the invite for `tc join` with no argument.
const inviteEnv = "TC_INVITE"

// parseJoinArgs accepts flags before or after the invite; envInvite
// ($TC_INVITE) is used when no invite is given.
func parseJoinArgs(args []string, envInvite string) (invite, name, key string, err error) {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&name, "name", "", "your user name (default: from git config user.name or $USER)")
	fs.StringVar(&key, "key", "", "SSH private key (default: ssh-agent ed25519, then ~/.ssh/id_ed25519, else a new tc key)")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		invite, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", "", "", errUsage
	}
	if invite == "" && fs.NArg() > 0 {
		invite = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return "", "", "", errUsage
		}
	}
	if fs.NArg() > 0 {
		return "", "", "", errUsage
	}
	if invite == "" {
		invite = strings.TrimSpace(envInvite)
	}
	if invite == "" {
		return "", "", "", errUsage
	}
	return invite, name, key, nil
}

func runJoin(ctx context.Context, env joinEnv, invite, name, keyPath string) error {
	inv, err := proto.ParseInvite(invite)
	if err != nil {
		return err
	}
	keyPath, err = chooseKey(env, keyPath)
	if err != nil {
		return err
	}

	in := bufio.NewReader(env.in)
	if name == "" {
		def := defaultJoinName(env.gitName(), env.user)
		if name, err = askName(env, in, def); err != nil {
			return err
		}
	} else if !proto.ValidName(name) {
		return fmt.Errorf("--name %q: use 1-32 of a-z 0-9 _ - (starting with a letter or digit)", sanitize.Line(name))
	}

	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fmt.Fprintf(env.out, "Joining %s as %s…\n", sanitize.Line(inv.Addr), name)
	u, err := env.login(lctx, env.home, invite, name, keyPath)
	var bye *client.ByeError
	// The one retry: a taken name doesn't spend the invite (PROTOCOL.md §4.1).
	if errors.As(err, &bye) && bye.Code == proto.ByeNameTaken && env.tty {
		fmt.Fprintf(env.out, "The name %s is taken.\n", name)
		if name, err = askName(env, in, ""); err != nil {
			return err
		}
		u, err = env.login(lctx, env.home, invite, name, keyPath)
	}
	if errors.As(err, &bye) && bye.Code == proto.ByeNameTaken {
		return fmt.Errorf("the name %s is taken; run again with --name another-name", name)
	}
	if err != nil {
		return err
	}
	printJoinTips(env, u)
	return nil
}

// chooseKey picks the login key and says which: the given --key, else the
// first ed25519 key in ssh-agent, else ~/.ssh/id_ed25519, else a tc-only key
// in TC_HOME (generated once, reused after). "" means "ask the agent".
func chooseKey(env joinEnv, keyPath string) (string, error) {
	if keyPath != "" {
		if _, err := identity.Load(keyPath); err != nil {
			return "", err
		}
		fmt.Fprintf(env.out, "Using SSH key %s\n", keyPath)
		return keyPath, nil
	}
	if pk, ok := identity.AgentED25519(); ok {
		fmt.Fprintf(env.out, "Using your ssh-agent key (%s)\n", identity.Fingerprint(pk))
		return "", nil
	}
	if env.sshDir != "" {
		p := filepath.Join(env.sshDir, "id_ed25519")
		if _, err := os.Stat(p); err == nil {
			if _, err := identity.Load(p); err != nil {
				return "", err // e.g. passphrase-protected: the message says ssh-add
			}
			fmt.Fprintf(env.out, "Using SSH key %s\n", p)
			return p, nil
		}
	}
	p := filepath.Join(env.home, "id_ed25519")
	if _, err := os.Stat(p); err == nil {
		if _, err := identity.Load(p); err != nil {
			return "", err
		}
		fmt.Fprintf(env.out, "Using tc's SSH key %s\n", p)
		return p, nil
	}
	if _, err := identity.GenerateKey(p); err != nil {
		return "", err
	}
	fmt.Fprintf(env.out, "No SSH key found, so tc made one just for itself: %s\n", p)
	fmt.Fprintln(env.out, "It has no passphrase: anyone who can read that file can chat as you. (Prefer your own key? tc join --key ~/.ssh/id_ed25519.)")
	return p, nil
}

// defaultJoinName is the first word of git's user.name, else $USER, made
// into a valid tc name; "" if neither works.
func defaultJoinName(gitName, user string) string {
	for _, c := range []string{firstField(gitName), user} {
		if n := sanitizeName(c); n != "" {
			return n
		}
	}
	return ""
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	n := strings.TrimLeft(b.String(), "_-")
	if len(n) > 32 {
		n = n[:32]
	}
	if !proto.ValidName(n) || strings.HasSuffix(n, proto.BotSuffix) {
		return ""
	}
	return n
}

// askName prompts for a name with def as the Enter default. Without a
// terminal it takes def (or fails if there is none).
func askName(env joinEnv, in *bufio.Reader, def string) (string, error) {
	if !env.tty {
		if def == "" {
			return "", errors.New("can't pick a user name (no git user.name or $USER); pass --name you")
		}
		return def, nil
	}
	for range 5 {
		if def != "" {
			fmt.Fprintf(env.out, "Your name in chat [%s]: ", def)
		} else {
			fmt.Fprint(env.out, "Your name in chat: ")
		}
		line, err := in.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			line = def
		}
		if line != "" && proto.ValidName(line) && !strings.HasSuffix(line, proto.BotSuffix) {
			return line, nil
		}
		if err != nil {
			return "", errors.New("no name given; pass --name you")
		}
		fmt.Fprintln(env.out, "Use 1-32 of a-z 0-9 _ - (starting with a letter or digit), not ending in -bot.")
	}
	return "", errors.New("no valid name given; pass --name you")
}

func printJoinTips(env joinEnv, u proto.User) {
	fmt.Fprintf(env.out, "\nYou're in as %s. Next:\n\n", sanitize.Line(u.Name))
	fmt.Fprintln(env.out, "  tc               start chatting")
	fmt.Fprintln(env.out, "  Ctrl-K / F1      jump to any channel or person / all keys")
	fmt.Fprintln(env.out, "  /dm @name        message someone directly")
	if env.herdr {
		fmt.Fprintln(env.out, "\nIn herdr: run tc in a pane; mentions become herdr notifications and the pane title shows unread counts.")
	}
}

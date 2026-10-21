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
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/crypto/ssh"

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

// isTerminal reports whether f is a terminal; /dev/null is a character
// device too, so the file mode alone isn't enough.
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd())
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

// cmdJoin: tc join|login [<invite>|tc://host:port#pin] [--name you] [--key path].
// Without an argument the invite comes from $TC_INVITE, which (unlike argv)
// other local users can't read in ps; install.sh passes it that way. With
// neither, it prints this computer's server line for use on another one.
func cmdJoin(args []string) error {
	envInvite := os.Getenv(inviteEnv)
	// Not passed on to anything tc runs (git, ssh-agent).
	os.Unsetenv(inviteEnv)
	if len(args) == 0 && strings.TrimSpace(envInvite) == "" {
		return showLogin(os.Stdout, client.DefaultHome())
	}
	invite, name, key, err := parseJoinArgs(args, envInvite)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	return runJoin(ctx, osJoinEnv(), invite, name, key)
}

// showLogin prints the tokenless server line (tc://host:port#pin) of the
// account in home: what `tc login` takes on a second computer.
func showLogin(out io.Writer, home string) error {
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		return multilineError("this computer isn't signed in to a tc server yet.\n" +
			"  New to the team? Run the invite your admin sent: tc join 'tc://…'\n" +
			"  Already have an account? Run tc login on a computer that's signed in; it prints the line to run here.")
	}
	c, err := client.Open(home)
	if err != nil {
		return err
	}
	cfg := c.Config()
	c.Close()
	line := proto.FormatServer(cfg.Addr, cfg.Pin)
	if !proto.ShellSafe(line) {
		return fmt.Errorf("the saved server address in %s isn't printable", home)
	}
	fmt.Fprintf(out, "This computer is signed in as %s to:\n\n  %s\n\n", sanitize.Line(cfg.Name), line)
	fmt.Fprintf(out, "On another computer that has the same SSH key (in ssh-agent or ~/.ssh), run:\n\n  tc login '%s'\n\n", line)
	fmt.Fprintln(out, "The account is the SSH key: copy that key to the other computer (or reach it through ssh-agent forwarding).")
	return nil
}

// inviteEnv holds the invite for `tc join` with no argument.
const inviteEnv = "TC_INVITE"

// parseJoinArgs accepts flags before or after the invite; envInvite
// ($TC_INVITE) is used when no invite is given.
func parseJoinArgs(args []string, envInvite string) (invite, name, key string, err error) {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&name, "name", "", "your user name (default: from git config user.name or $USER)")
	fs.StringVar(&key, "key", "", "SSH key: a private key file, or a .pub whose private half is in ssh-agent (default: try every agent key and ~/.ssh/id_*)")
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

// joinKey is one key runJoin may try.
type joinKey struct {
	identity.Candidate
	tcKey bool // TC_HOME/id_ed25519, made by an earlier join
}

func (k joinKey) announce(env joinEnv) {
	switch {
	case k.tcKey:
		fmt.Fprintf(env.out, "Using tc's SSH key %s\n", k.Path)
	case k.Path == "":
		fmt.Fprintf(env.out, "Using your ssh-agent key (%s)\n", identity.Fingerprint(k.Pub))
	default:
		fmt.Fprintf(env.out, "Using SSH key %s\n", k.Path)
	}
}

// runJoin redeems an invite, or (code without a token) logs in a key that
// is already registered. Without --key it tries the candidate keys in
// order. A key-bound invite never costs a guess per key: the server says
// which fingerprints it accepts (key_hint) and nothing is spent until the
// right key is used. A key that is already registered logs in whatever
// the invite, so retrying an interrupted join works.
func runJoin(ctx context.Context, env joinEnv, code, name, keyPath string) error {
	inv, err := proto.ParseLogin(code)
	if err != nil {
		return err
	}
	return joinErrorHint(inv, joinWithKeys(ctx, env, inv, code, name, keyPath))
}

func joinWithKeys(ctx context.Context, env joinEnv, inv proto.Invite, code, name, keyPath string) error {
	loginOnly := inv.Token == ""
	keys, all, err := joinKeys(env, keyPath)
	if err != nil {
		return err
	}
	generated := ""
	if len(keys) == 0 {
		if loginOnly {
			return noKeyError(all, nil)
		}
		p := filepath.Join(env.home, "id_ed25519")
		s, err := identity.GenerateKey(p)
		if err != nil {
			return err
		}
		generated = p
		keys = []joinKey{{Candidate: identity.Candidate{Pub: s.PublicKey(), Signer: s, Path: p}}}
	}
	// A generated key the server refused is worthless; don't leave it. On
	// any other failure (timeout, lost WELCOME) the server may have
	// registered it, and a retry needs it.
	keep := false
	var lastErr error
	defer func() {
		var bye *client.ByeError
		refused := errors.As(lastErr, &bye) && bye.Code != proto.ByeKeyExists
		if generated != "" && !keep && refused {
			os.Remove(generated)
			os.Remove(generated + ".pub")
		}
	}()

	in := bufio.NewReader(env.in)
	if loginOnly {
		name = ""
	} else if name == "" {
		def := defaultJoinName(env.gitName(), env.user)
		if name, err = askName(env, in, def); err != nil {
			return err
		}
	} else if !proto.ValidName(name) {
		return fmt.Errorf("--name %q: use 1-32 of a-z 0-9 _ - (starting with a letter or digit)", sanitize.Line(name))
	}

	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if loginOnly {
		fmt.Fprintf(env.out, "Logging in to %s…\n", sanitize.Line(inv.Addr))
	} else {
		fmt.Fprintf(env.out, "Joining %s as %s…\n", sanitize.Line(inv.Addr), name)
	}
	var u proto.User
	var used joinKey
	hinted := false
	blind := 0
	for i := 0; i < len(keys); i++ {
		k := keys[i]
		// Keys not picked by the server's hint are offered blind: cap them,
		// and leave security keys (a touch each) to --key or the hint, so a
		// hostile server can't harvest every key or train prompt approval.
		if !hinted {
			if blind > 0 && (blind >= maxBlindKeys || strings.HasPrefix(k.Pub.Type(), "sk-")) {
				continue
			}
			blind++
		}
		if generated == "" {
			k.announce(env)
		}
		u, err = env.login(lctx, env.home, code, name, k.Spec())
		var bye *client.ByeError
		// The one name retry: a taken name doesn't spend the invite (§4.1).
		if errors.As(err, &bye) && bye.Code == proto.ByeNameTaken && env.tty {
			fmt.Fprintf(env.out, "The name %s is taken.\n", name)
			if name, err = askName(env, in, ""); err != nil {
				return err
			}
			u, err = env.login(lctx, env.home, code, name, k.Spec())
		}
		if errors.As(err, &bye) && bye.Code == proto.ByeNameTaken {
			return fmt.Errorf("the name %s is taken; run again with --name another-name", name)
		}
		lastErr = err
		if err == nil {
			used = k
			break
		}
		if !errors.As(err, &bye) {
			return err
		}
		switch {
		case bye.Code == proto.ByeInviteInvalid && len(bye.Keys) > 0:
			// Key-bound, and the server said which keys: jump straight to ours.
			if hinted {
				return err
			}
			hinted = true
			next, ok, herr := hintedKey(bye.Keys, all, keys[:i+1])
			if herr != nil {
				return herr
			}
			if !ok {
				return keyBoundError(bye.Keys, all, keyPath)
			}
			keys = append(keys[:i+1], next)
		case bye.Code == proto.ByeInviteInvalid && strings.Contains(bye.Msg, "other SSH keys"):
			// A v0.2.0 server (no key_hint): try the next key, nothing spent.
			// Keying on msg is only for those frozen servers.
		case bye.Code == proto.ByeAuthFailed && loginOnly:
			// Not this key's account: try the next.
		default:
			return err
		}
	}
	if u.ID == 0 {
		if loginOnly {
			return noKeyError(all, lastErr)
		}
		var bye *client.ByeError
		if errors.As(lastErr, &bye) && bye.Code == proto.ByeInviteInvalid && keyPath == "" {
			return keyBoundError(nil, all, keyPath)
		}
		return lastErr
	}
	if generated != "" {
		keep = true
		if locked := unusableKeys(all); len(locked) > 0 {
			fmt.Fprintf(env.out, "tc can't use your SSH key %s (passphrase-protected and not in ssh-agent, or unreadable), so it made one just for itself: %s\n",
				strings.Join(locked, ", "), generated)
			fmt.Fprintf(env.out, "To use yours instead, unlock it with ssh-add %s and run this join again with --key %s.\n", locked[0], locked[0])
		} else {
			fmt.Fprintf(env.out, "No SSH key found, so tc made one just for itself: %s\n", generated)
		}
		fmt.Fprintln(env.out, "It has no passphrase: anyone who can read that file can chat as you. (Prefer your own key? tc join --key ~/.ssh/id_ed25519.)")
	}
	if !loginOnly && (u.Name != name || preexisting(u, time.Now())) {
		fmt.Fprintf(env.out, "Note: %s already has an account, so you're signed in as %s and the invite was not used (it grants nothing to an existing key).\n",
			sanitize.Line(used.Describe()), sanitize.Line(u.Name))
	}
	printJoinTips(env, u)
	return nil
}

// preexisting reports whether u was created well before this join, i.e. a
// registered key logged in and the invite was ignored (§4). A join retried
// after a lost WELCOME finds a minutes-old account and says nothing.
func preexisting(u proto.User, now time.Time) bool {
	return u.Created > 0 && now.Sub(time.UnixMilli(u.Created)) > 15*time.Minute
}

// unusableKeys lists the key files found that can't sign here, e.g. ones
// with a passphrase that aren't in ssh-agent.
func unusableKeys(all []joinKey) []string {
	var out []string
	for _, k := range all {
		if k.Signer == nil && k.Path != "" {
			out = append(out, sanitize.Line(k.Path))
		}
	}
	return out
}

// maxBlindKeys is how many keys runJoin offers without a key hint.
const maxBlindKeys = 3

// joinKeys returns the keys to try, in order, and every candidate found
// (including ones that can't sign here, for error messages). --key gives
// exactly one. Otherwise: ed25519 keys first (agent, then ~/.ssh), then
// the others (ECDSA, RSA, security keys), then tc's own key from an
// earlier join.
func joinKeys(env joinEnv, keyPath string) (try, all []joinKey, err error) {
	if keyPath != "" {
		s, err := identity.Load(keyPath)
		if err != nil {
			return nil, nil, err
		}
		k := joinKey{Candidate: identity.Candidate{Pub: s.PublicKey(), Signer: s, Path: keyPath}}
		return []joinKey{k}, []joinKey{k}, nil
	}
	for _, c := range identity.Candidates(env.sshDir) {
		all = append(all, joinKey{Candidate: c})
	}
	p := filepath.Join(env.home, "id_ed25519")
	if _, err := os.Stat(p); err == nil {
		s, err := identity.Load(p)
		if err != nil {
			return nil, nil, err
		}
		dup := false
		for _, k := range all {
			dup = dup || identity.Fingerprint(k.Pub) == identity.Fingerprint(s.PublicKey())
		}
		if !dup {
			all = append(all, joinKey{Candidate: identity.Candidate{Pub: s.PublicKey(), Signer: s, Path: p}, tcKey: true})
		}
	}
	for _, ed := range []bool{true, false} {
		for _, k := range all {
			if k.Signer != nil && !k.tcKey && (k.Pub.Type() == ssh.KeyAlgoED25519) == ed {
				try = append(try, k)
			}
		}
	}
	for _, k := range all {
		if k.tcKey {
			try = append(try, k)
		}
	}
	return try, all, nil
}

// hintedKey finds the candidate whose fingerprint the invite accepts and
// that wasn't tried yet. A match that can't sign here (a locked key file)
// is an error that says how to unlock it.
func hintedKey(fps []string, all, tried []joinKey) (joinKey, bool, error) {
	for _, k := range all {
		if !slices.Contains(fps, identity.Fingerprint(k.Pub)) {
			continue
		}
		if slices.ContainsFunc(tried, func(t joinKey) bool { return identity.Fingerprint(t.Pub) == identity.Fingerprint(k.Pub) }) {
			continue
		}
		if k.Signer == nil {
			return joinKey{}, false, fmt.Errorf("this invite is for your key %s, but tc can't use it: %w", k.Describe(), k.Err)
		}
		return k, true, nil
	}
	return joinKey{}, false, nil
}

// keyBoundError explains a key-bound invite none of our keys can redeem.
func keyBoundError(fps []string, all []joinKey, keyPath string) error {
	var b strings.Builder
	b.WriteString("this invite only works with particular SSH keys, and ")
	if keyPath != "" {
		fmt.Fprintf(&b, "%s isn't one of them", keyPath)
	} else {
		b.WriteString("none of yours is one of them")
	}
	if len(fps) > 0 {
		fps = fps[:min(len(fps), proto.MaxInviteKeys)]
		b.WriteString("\n  it accepts: " + strings.Join(sanitizeAll(fps), ", "))
	}
	writeTried(&b, all)
	b.WriteString("\n  if the key is elsewhere (another computer, a hardware key, a password manager), load it into ssh-agent or pass --key path/to/key(.pub); otherwise ask for a new invite")
	return multilineError(b.String())
}

// noKeyError explains a tc login with no registered key.
func noKeyError(all []joinKey, last error) error {
	var b strings.Builder
	b.WriteString("none of your SSH keys has an account on this server")
	if len(all) == 0 {
		b.WriteString(" (no keys found in ssh-agent or ~/.ssh)")
	}
	writeTried(&b, all)
	b.WriteString("\n  tc login needs the key you joined with: load it into ssh-agent (ssh-add) or pass --key path; the account belongs to that key, so copy it here from the computer you joined on")
	if last != nil {
		var bye *client.ByeError
		if !errors.As(last, &bye) || bye.Code != proto.ByeAuthFailed {
			b.WriteString("\n  last error: " + last.Error())
		}
	}
	return multilineError(b.String())
}

func writeTried(b *strings.Builder, all []joinKey) {
	for _, k := range all {
		fmt.Fprintf(b, "\n  - %s (%s)", sanitize.Line(k.Describe()), identity.Fingerprint(k.Pub))
		if k.Err != nil {
			fmt.Fprintf(b, ": %s", sanitize.Line(k.Err.Error()))
		}
	}
}

func sanitizeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitize.Line(s)
	}
	return out
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
	fmt.Fprintln(env.out, "  Option-K / F1    command palette: jump anywhere, run anything / all keys")
	fmt.Fprintln(env.out, "  /dm @name        message someone directly")
	if env.herdr {
		fmt.Fprintln(env.out, "\nIn herdr: run tc in a pane; mentions become herdr notifications and the pane title shows unread counts.")
	}
}

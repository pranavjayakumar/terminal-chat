package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/snippet"
)

func runClient(cmd string, args []string) error {
	switch cmd {
	case "":
		return cmdChat()
	case "join", "login":
		return cmdJoin(args)
	case "send":
		return cmdSend(args)
	case "tail":
		return cmdTail(args)
	case "notify":
		return cmdNotify(args)
	case "verify":
		return cmdVerify(args)
	case "devices":
		return cmdDevices(args)
	case "invite":
		return cmdInvite(args)
	case "users":
		return cmdUsers(args)
	case "background":
		return cmdBackground(args)
	}
	return errUsage
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// connected opens the client, runs it, and waits until it's online.
func connected(ctx context.Context) (*client.Client, error) {
	c, err := client.Open(client.DefaultHome())
	if err != nil {
		return nil, err
	}
	go c.Run(ctx)
	deadline := time.After(15 * time.Second)
	for {
		st := c.Status()
		if st.State == client.Online {
			return c, nil
		}
		if st.Stopped {
			c.Close()
			return nil, stoppedError(st.Err)
		}
		select {
		case <-ctx.Done():
			c.Close()
			return nil, ctx.Err()
		case <-deadline:
			c.Close()
			addr := sanitize.Line(c.Config().Addr)
			if st.Err != "" {
				return nil, fmt.Errorf("can't reach the server at %s: it may be down or blocked by a firewall; try again later or ask your admin (%s)", addr, st.Err)
			}
			return nil, fmt.Errorf("can't reach the server at %s: it may be down or blocked by a firewall; try again later or ask your admin", addr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stoppedError explains why the client gave up for good. Status carries
// only the error text, so the server's stable bye codes are matched on it.
func stoppedError(msg string) error {
	switch msg {
	case (&client.ByeError{Code: proto.ByeRevoked}).Error():
		return fmt.Errorf("%s: %s", revokedHint, msg)
	case (&client.ByeError{Code: proto.ByeAuthFailed}).Error():
		return fmt.Errorf("the server no longer accepts this computer's SSH key; if you joined with a different key, run tc login with it (%s)", msg)
	}
	if strings.Contains(msg, "pin mismatch") {
		return fmt.Errorf("the server's identity changed since you joined (its pin doesn't match): if your admin reinstalled it, ask for a fresh invite; otherwise something may be intercepting the connection (%s)", msg)
	}
	return errors.New(msg)
}

// revokedHint is what a disabled account or revoked device is told.
const revokedHint = "this account was disabled by an admin, or this device was revoked, so it can't sign in. Ask an admin; a disabled account needs a new invite and a new SSH key"

// resolve turns "#chan" or "@user" into a channel id, opening a DM if needed.
func resolve(ctx context.Context, c *client.Client, target string) (uint64, error) {
	switch {
	case strings.HasPrefix(target, "#"):
		name := target[1:]
		for _, ch := range c.Channels() {
			if ch.Name == name && ch.Kind != proto.ChanDM {
				if !ch.Member {
					return 0, fmt.Errorf("you're not in #%s (join it in tc first)", name)
				}
				return ch.ID, nil
			}
		}
		return 0, fmt.Errorf("no channel #%s", name)
	case target == "@me":
		notes, err := c.OpenNotes(ctx)
		return notes.ID, err
	case strings.HasPrefix(target, "@"):
		name := target[1:]
		for _, u := range c.Users() {
			if u.Name == name {
				dm, err := c.OpenDM(ctx, []uint64{u.ID})
				return dm.ID, err
			}
		}
		return 0, fmt.Errorf("no user @%s", name)
	}
	return 0, fmt.Errorf("target must be #channel, @user or @me, got %q", target)
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	raw := fs.Bool("raw", false, "don't wrap piped input in a code block")
	file := fs.String("file", "", "send a file as a code block: path[:start[-end]] (message text goes above it)")
	ctxLines := fs.Int("context", snippet.DefaultContext, "lines of context around a single --file line")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errUsage
	}
	target := args[0]
	fs.Parse(args[1:])

	// Like piped input: `tc send '#ci' "$(make 2>&1)"` often carries colors,
	// which the server would reject as control characters.
	body := sanitize.StripANSI(strings.Join(fs.Args(), " "))
	if *file != "" {
		if *ctxLines < 0 {
			return errors.New("--context must be 0 or more")
		}
		var err error
		if body, err = snippet.Build(snippet.Options{Ref: *file, Context: *ctxLines, Message: body}); err != nil {
			return err
		}
	} else if body == "" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
		if err != nil {
			return err
		}
		if strings.TrimSpace(sanitize.StripANSI(string(b))) == "" {
			return errors.New("nothing to send") // else pipedBody wraps it in an empty code block
		}
		body = pipedBody(string(b), *raw)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("nothing to send")
	}

	ctx, cancel := signalContext()
	defer cancel()
	return postTo(ctx, target, body)
}

// pipedBody prepares stdin for sending: strips terminal control sequences
// (logs are often colored), keeps the tail if it's too long, and wraps it in
// a code block so it renders as a snippet. A unified diff gets a diff fence
// so it renders with colors.
func pipedBody(s string, raw bool) string {
	s = strings.TrimRight(sanitize.StripANSI(s), "\n")
	limit := proto.MaxBody - 64
	if len(s) > limit {
		cut := len(s) - limit
		for cut < len(s) && s[cut]&0xC0 == 0x80 { // don't split a UTF-8 sequence
			cut++
		}
		s = fmt.Sprintf("… (%d bytes truncated)\n%s", cut, s[cut:])
	}
	if raw {
		return s
	}
	lang := ""
	if snippet.LooksLikeDiff(s) {
		lang = "diff"
	}
	return "```" + lang + "\n" + strings.ReplaceAll(s, "```", "ʼʼʼ") + "\n```"
}

func cmdTail(args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	ctx, cancel := signalContext()
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	ch, err := resolve(ctx, c, args[0])
	if err != nil {
		return err
	}
	return tail(ctx, c, ch, os.Stdout)
}

// tail prints ch's recent messages, then new ones as they arrive.
// Encrypted messages arrive decrypted; one still waiting for its key is
// printed once it opens (it comes again in Updates().New).
func tail(ctx context.Context, c *client.Client, ch uint64, w io.Writer) error {
	seen := map[uint64]bool{}
	for _, m := range c.Messages(ch, 20) {
		if m.ID != 0 {
			seen[m.ID] = m.Sec != client.SecWaiting
			printMsg(w, c, m)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-c.Updates():
			if !ok {
				return nil
			}
			for _, m := range u.New {
				if m.Chan == ch && !seen[m.ID] {
					seen[m.ID] = true
					printMsg(w, c, m)
				}
			}
			if st := c.Status(); st.Stopped {
				return errors.New(st.Err)
			}
		}
	}
}

func printMsg(w io.Writer, c *client.Client, m client.Message) {
	t := time.UnixMilli(m.Time).Format("15:04")
	name := sanitize.Line(c.User(m.Author).Name)
	body := sanitize.Text(m.Body)
	switch {
	case m.Deleted && m.Sec == client.SecWaiting:
		body = "(deleted, unverified)"
	case m.Deleted && m.Sec != client.SecPlain && m.Sec != client.SecE2E:
		body = "(removed, not by author)"
	case m.Deleted:
		body = "(deleted)"
	case m.Sec == client.SecWaiting && m.Wait == client.WaitKeyChange && m.Author == c.Me().ID:
		body = "🔒 sent under an SSH key of yours this device doesn't trust"
	case m.Sec == client.SecWaiting && m.Wait == client.WaitKeyChange:
		body = "🔒 @" + name + "'s key changed: accept it with /accept-key @" + name + " in tc"
	case m.Sec == client.SecWaiting:
		body = "🔒 waiting for key"
	case m.Sec == client.SecInvalid:
		body = "⚠ invalid encrypted message (not shown)"
	case m.Sec == client.SecInserted:
		body = "⚠ inserted by server: " + body
	case m.Sec == client.SecLegacy:
		body = "⚠ unauthenticated (legacy): " + body
	case m.Sec == client.SecLegacyClaimed:
		body = "⚠ unauthenticated (legacy, server-claimed): " + body
	}
	if m.Thread != 0 {
		name += " ↳"
	}
	indent := strings.Repeat(" ", len(t)+len(name)+3)
	fmt.Fprintf(w, "%s %s: %s\n", t, name, strings.ReplaceAll(body, "\n", "\n"+indent))
}

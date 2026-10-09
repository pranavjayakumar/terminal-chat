package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

func runClient(cmd string, args []string) error {
	switch cmd {
	case "":
		return cmdChat()
	case "login":
		return cmdLogin(args)
	case "send":
		return cmdSend(args)
	case "tail":
		return cmdTail(args)
	case "notify":
		return cmdNotify(args)
	}
	return errUsage
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdLogin(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errUsage
	}
	invite := args[0]
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	name := fs.String("name", defaultName(), "your user name (new accounts)")
	key := fs.String("key", "", "SSH private key (default: ssh-agent, then ~/.ssh/id_ed25519)")
	fs.Parse(args[1:])

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	u, err := client.Login(ctx, client.DefaultHome(), invite, *name, *key)
	if err != nil {
		return err
	}
	fmt.Printf("Logged in as %s. Run `tc` to start chatting.\n", sanitize.Line(u.Name))
	return nil
}

func defaultName() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	n := strings.ToLower(u.Username)
	if proto.ValidName(n) {
		return n
	}
	return ""
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
			return nil, errors.New(st.Err)
		}
		select {
		case <-ctx.Done():
			c.Close()
			return nil, ctx.Err()
		case <-deadline:
			c.Close()
			if st.Err != "" {
				return nil, fmt.Errorf("can't reach server: %s", st.Err)
			}
			return nil, errors.New("can't reach server")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

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
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errUsage
	}
	target := args[0]
	fs.Parse(args[1:])

	body := strings.Join(fs.Args(), " ")
	if body == "" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
		if err != nil {
			return err
		}
		body = pipedBody(string(b), *raw)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("nothing to send")
	}

	ctx, cancel := signalContext()
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	ch, err := resolve(ctx, c, target)
	if err != nil {
		return err
	}
	if err := c.Send(ch, body, 0); err != nil {
		return err
	}
	fctx, fcancel := context.WithTimeout(ctx, 20*time.Second)
	defer fcancel()
	return c.Flush(fctx)
}

// pipedBody prepares stdin for sending: strips terminal control sequences
// (logs are often colored), keeps the tail if it's too long, and wraps it in
// a code block so it renders as a snippet.
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
	return "```\n" + strings.ReplaceAll(s, "```", "ʼʼʼ") + "\n```"
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
	seen := map[uint64]bool{}
	for _, m := range c.Messages(ch, 20) {
		if m.ID != 0 {
			seen[m.ID] = true
			printMsg(c, m.Message)
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
					printMsg(c, m)
				}
			}
			if st := c.Status(); st.Stopped {
				return errors.New(st.Err)
			}
		}
	}
}

func printMsg(c *client.Client, m proto.Message) {
	t := time.UnixMilli(m.Time).Format("15:04")
	name := sanitize.Line(c.User(m.Author).Name)
	body := sanitize.Text(m.Body)
	if m.Deleted {
		body = "(deleted)"
	}
	if m.Thread != 0 {
		name += " ↳"
	}
	indent := strings.Repeat(" ", len(t)+len(name)+3)
	fmt.Printf("%s %s: %s\n", t, name, strings.ReplaceAll(body, "\n", "\n"+indent))
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

const publicAddrFile = "public_addr"

func cmdServer(args []string) error {
	if len(args) > 0 && args[0] == "invite" {
		return cmdServerInvite(args[1:])
	}
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory (database, TLS key)")
	listen := fs.String("listen", ":7443", "listen address (UDP for QUIC and TCP)")
	public := fs.String("public", "", "host:port clients use to reach this server (saved for invites)")
	fs.Parse(args)

	if *public != "" {
		if err := server.ValidPublicAddr(*public); err != nil {
			return fmt.Errorf("--public: %w", err)
		}
		if err := os.MkdirAll(*data, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*data, publicAddrFile), []byte(*public), 0o600); err != nil {
			return err
		}
	}
	pub := readPublic(*data, *listen)
	if *public == "" {
		// Remember the address so `tc server invite` (a separate process) uses it too.
		if _, err := os.Stat(filepath.Join(*data, publicAddrFile)); os.IsNotExist(err) {
			os.MkdirAll(*data, 0o700)
			os.WriteFile(filepath.Join(*data, publicAddrFile), []byte(pub), 0o600)
		}
	}
	if server.TamperBuild {
		return errors.New("this tc binary was built with -tags tctamper (malicious-server test hooks); refusing to run a server")
	}
	srv, err := server.New(server.Config{DataDir: *data, PublicAddr: pub})
	if err != nil {
		return err
	}
	defer srv.Close()
	ln, err := transport.ListenWith(*listen, srv.Cert(), transport.ListenOpts{HTTP: true, HTTPCert: srv.HTTPCert()})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("tc server listening", "addr", ln.Addr().String(), "public", pub, "pin", srv.Pin(), "data", *data)
	if strings.HasPrefix(pub, "localhost:") {
		slog.Warn("invites point at localhost; pass --public host:port so teammates can connect")
	}
	if first, _ := firstRun(srv); first {
		inv, err := srv.CreateInviteOpts(ctx, server.InviteOpts{Admin: true})
		if err == nil {
			fmt.Println("\nNo users yet. Admin invite for you (single use, 24 hours):")
			printInvite(os.Stdout, inv, version)
			fmt.Println("Then invite your team with: tc server invite --uses 15 --ttl 24h")
		}
	}
	return srv.Serve(ctx, ln)
}

// serverInviteCmd is a parsed `tc server invite`.
type serverInviteCmd struct {
	data   string
	opts   server.InviteOpts
	raw    bool
	list   bool   // --list: show open invites instead
	revoke string // --revoke ID
}

func parseServerInvite(args []string) (serverInviteCmd, error) {
	var c serverInviteCmd
	fs := flag.NewFlagSet("server invite", flag.ContinueOnError)
	fs.StringVar(&c.data, "data", defaultDataDir(), "data directory")
	fs.BoolVar(&c.opts.Admin, "admin", false, "invite grants admin (single use, at most 24h)")
	fs.IntVar(&c.opts.Uses, "uses", 1, fmt.Sprintf("how many people can join with it (1-%d)", proto.MaxInviteUses))
	fs.DurationVar(&c.opts.TTL, "ttl", proto.DefaultInviteTTL, "how long it stays valid (at most 720h; admin: 24h)")
	fs.BoolVar(&c.raw, "raw", false, "print only the tc:// code")
	fs.BoolVar(&c.list, "list", false, "list open invites (ids, kind, uses left, expiry, creator); tokens are never shown")
	fs.StringVar(&c.revoke, "revoke", "", "make the invite with this id (from --list) unusable at once")
	if err := fs.Parse(args); err != nil {
		return c, errUsage
	}
	if fs.NArg() > 0 {
		return c, errUsage
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if c.list || set["revoke"] {
		if c.list && set["revoke"] {
			return c, errors.New("use --list or --revoke, not both")
		}
		for _, f := range []string{"admin", "uses", "ttl", "raw"} {
			if set[f] {
				return c, fmt.Errorf("--%s doesn't go with --list or --revoke", f)
			}
		}
		c.revoke = strings.ToLower(strings.TrimSpace(c.revoke))
		if set["revoke"] && !proto.ValidInviteID(c.revoke) {
			return c, fmt.Errorf("--revoke takes the %d-character id from --list", proto.InviteIDLen)
		}
		return c, nil
	}
	if c.opts.Admin {
		if c.opts.Uses != 1 {
			return c, errors.New("--admin invites are single-use: drop --uses")
		}
		if !set["ttl"] {
			c.opts.TTL = proto.MaxAdminInviteTTL
		} else if c.opts.TTL > proto.MaxAdminInviteTTL {
			return c, errors.New("--admin invites last at most 24h")
		}
	}
	return c, nil
}

func cmdServerInvite(args []string) error {
	c, err := parseServerInvite(args)
	if err != nil {
		return err
	}
	srv, err := server.New(server.Config{DataDir: c.data, PublicAddr: readPublic(c.data, ":7443"),
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		return err
	}
	defer srv.Close()
	ctx := context.Background()
	switch {
	case c.list:
		invs, err := srv.ListInvites(ctx)
		if err != nil {
			return err
		}
		printInvites(os.Stdout, invs, func(id uint64) string { return srv.UserName(ctx, id) })
		return nil
	case c.revoke != "":
		if err := srv.RevokeInvite(ctx, c.revoke); err != nil {
			return err
		}
		fmt.Printf("Revoked invite %s; it can't be used any more.\n", c.revoke)
		return nil
	}
	inv, err := srv.CreateInviteOpts(ctx, c.opts)
	if err != nil {
		return err
	}
	if c.raw {
		fmt.Println(inv.Code)
		return nil
	}
	printInvite(os.Stdout, inv, version)
	return nil
}

// printInvite shows an invite as the install one-liner (for people without
// tc, pinned to version's install.sh) and as the raw code (for `tc join`).
func printInvite(w io.Writer, inv proto.InviteRes, version string) {
	what := "single use"
	if inv.Uses > 1 {
		what = fmt.Sprintf("%d uses", inv.Uses)
	}
	if inv.Expires > 0 {
		what += ", expires " + time.UnixMilli(inv.Expires).Format("Mon Jan 2 15:04 MST")
	}
	// The code embeds --public; refuse to print anything that isn't plain
	// and shell-safe rather than a line that would do something else.
	if !proto.ShellSafe(inv.Code) {
		fmt.Fprintf(w, "\nInvite (%s) has unsafe characters, so it isn't shown as a command: %s\n"+
			"Check --public (letters, digits, '.', '-', ':' and [] only).\n\n", what, sanitize.Line(inv.Code))
		return
	}
	fmt.Fprintf(w, "\nInvite (%s). Share this line; it installs tc and joins:\n\n", what)
	if cmd, err := proto.InstallCommand(inv.Code, version); err == nil {
		fmt.Fprintf(w, "  %s\n\n", sanitize.Line(cmd))
	}
	fmt.Fprintf(w, "Already have tc? Run:\n\n  tc join '%s'\n\n", sanitize.Line(inv.Code))
}

// printInvites is the --list table. Names come from the server's own
// database but still pass through sanitize.Line.
func printInvites(w io.Writer, invs []proto.InviteInfo, name func(uint64) string) {
	if len(invs) == 0 {
		fmt.Fprintln(w, "No open invites.")
		return
	}
	fmt.Fprintf(w, "%-12s  %-14s  %9s  %-17s  %s\n", "ID", "KIND", "USES LEFT", "EXPIRES", "CREATED BY")
	for _, inv := range invs {
		kind := sanitize.Line(inv.Kind)
		if inv.Kind == proto.InviteKindKey {
			kind = "key for " + who(inv.User, name)
		}
		by := "server host"
		if inv.CreatedBy != 0 {
			by = who(inv.CreatedBy, name)
		}
		fmt.Fprintf(w, "%-12s  %-14s  %9d  %-17s  %s\n", sanitize.Line(inv.ID), kind, inv.UsesLeft,
			time.UnixMilli(inv.Expires).Format("Jan 2 15:04 MST"), by)
	}
	fmt.Fprintln(w, "\nRevoke one with: tc server invite --revoke ID")
}

func who(id uint64, name func(uint64) string) string {
	if n := sanitize.Line(name(id)); n != "" {
		return "@" + n
	}
	return fmt.Sprintf("user %d", id)
}

func firstRun(srv *server.Server) (bool, error) { return srv.UserCount() == 0, nil }

// readPublic returns the saved public address, falling back to the listen
// address with localhost.
func readPublic(data, listen string) string {
	if b, err := os.ReadFile(filepath.Join(data, publicAddrFile)); err == nil {
		return strings.TrimSpace(string(b))
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func defaultDataDir() string {
	if d := os.Getenv("TC_DATA"); d != "" {
		return d
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".local", "share", "tc-server")
	}
	return "tc-data"
}

var errUsage = errors.New("usage")

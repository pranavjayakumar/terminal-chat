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
			fmt.Println("\nNo users yet. Admin invite for you (single use, 7 days):")
			printInvite(os.Stdout, inv)
			fmt.Println("Then invite your team with: tc server invite --uses 15 --ttl 72h")
		}
	}
	return srv.Serve(ctx, ln)
}

func cmdServerInvite(args []string) error {
	fs := flag.NewFlagSet("server invite", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	admin := fs.Bool("admin", false, "invite grants admin")
	uses := fs.Int("uses", 1, fmt.Sprintf("how many people can join with it (1-%d)", proto.MaxInviteUses))
	ttl := fs.Duration("ttl", proto.DefaultInviteTTL, "how long it stays valid (at most 720h)")
	raw := fs.Bool("raw", false, "print only the tc:// code")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return errUsage
	}
	srv, err := server.New(server.Config{DataDir: *data, PublicAddr: readPublic(*data, ":7443"),
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		return err
	}
	defer srv.Close()
	inv, err := srv.CreateInviteOpts(context.Background(), server.InviteOpts{Admin: *admin, Uses: *uses, TTL: *ttl})
	if err != nil {
		return err
	}
	if *raw {
		fmt.Println(inv.Code)
		return nil
	}
	printInvite(os.Stdout, inv)
	return nil
}

// printInvite shows an invite as the install one-liner (for people without
// tc) and as the raw code (for `tc join`).
func printInvite(w io.Writer, inv proto.InviteRes) {
	what := "single use"
	if inv.Uses > 1 {
		what = fmt.Sprintf("%d uses", inv.Uses)
	}
	if inv.Expires > 0 {
		what += ", expires " + time.UnixMilli(inv.Expires).Format("Mon Jan 2 15:04 MST")
	}
	fmt.Fprintf(w, "\nInvite (%s). Share this line; it installs tc and joins:\n\n", what)
	if cmd, err := proto.InstallCommand(inv.Code); err == nil {
		fmt.Fprintf(w, "  %s\n\n", cmd)
	}
	fmt.Fprintf(w, "Already have tc? Run:\n\n  tc join '%s'\n\n", inv.Code)
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

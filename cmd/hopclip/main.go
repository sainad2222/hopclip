package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/sainad2222/hopclip/internal/auth"
	"github.com/sainad2222/hopclip/internal/config"
	"github.com/sainad2222/hopclip/internal/server"
	"github.com/sainad2222/hopclip/internal/store"
	"github.com/sainad2222/hopclip/web"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage:
  hopclip [serve]                 run the server
  hopclip user list               list users
  hopclip user add NAME [-admin]  create a user (prompts for password)
  hopclip user passwd NAME        set a user's password and sign them out
  hopclip user admin NAME on|off  grant or revoke admin
  hopclip user del NAME           delete a user and all their data
  hopclip healthcheck             exit 0 if the local server is healthy
  hopclip version                 print the version

Passwords are read from the terminal, or from the first line of stdin when
it is not a terminal. All settings come from environment variables; see
.env.example.
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "user":
		err = userCmd(args)
	case "healthcheck":
		err = healthcheck()
	case "version", "--version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func openStore() (*config.Config, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open data dir %s: %w", cfg.DataDir, err)
	}
	return cfg, st, nil
}

func serve() error {
	cfg, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting hopclip", "version", version, "data_dir", cfg.DataDir)
	if err := bootstrapAdmin(ctx, cfg, st); err != nil {
		return err
	}
	if !cfg.CookieSecure {
		slog.Warn("COOKIE_SECURE=false: session cookies will be sent over plain HTTP; use only for local testing")
	}
	srv, err := server.New(cfg, st, web.Static())
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// bootstrapAdmin creates the ADMIN_USERNAME account on first start. An
// existing account is left alone so that a password changed in the UI is not
// reset on every restart.
func bootstrapAdmin(ctx context.Context, cfg *config.Config, st *store.Store) error {
	if cfg.AdminUsername == "" {
		if n, err := st.CountUsers(ctx); err != nil {
			return err
		} else if n == 0 {
			slog.Warn("no users exist; set ADMIN_USERNAME/ADMIN_PASSWORD or run `hopclip user add NAME -admin`")
		}
		return nil
	}
	_, err := st.UserByName(ctx, cfg.AdminUsername)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := server.ValidateUsername(cfg.AdminUsername); err != nil {
		return fmt.Errorf("ADMIN_USERNAME: %w", err)
	}
	if err := auth.ValidatePassword(cfg.AdminPassword); err != nil {
		return fmt.Errorf("ADMIN_PASSWORD: %w", err)
	}
	hash, err := auth.HashPassword(cfg.AdminPassword)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, cfg.AdminUsername, hash, true); err != nil {
		return err
	}
	slog.Info("created admin user", "username", cfg.AdminUsername)
	return nil
}

func userCmd(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing user subcommand")
	}
	_, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	sub, args := args[0], args[1:]
	if sub == "list" {
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSERNAME\tADMIN\tCREATED")
		for _, u := range users {
			fmt.Fprintf(tw, "%d\t%s\t%v\t%s\n", u.ID, u.Username, u.IsAdmin, time.UnixMilli(u.CreatedAt).Format(time.DateTime))
		}
		return tw.Flush()
	}

	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	isAdmin := fs.Bool("admin", false, "grant admin rights")
	// Accept the flag before or after NAME.
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	if len(positional) == 0 {
		return fmt.Errorf("user %s: missing NAME", sub)
	}
	name := positional[0]

	switch sub {
	case "add":
		if err := server.ValidateUsername(name); err != nil {
			return err
		}
		pw, err := readNewPassword()
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if _, err := st.CreateUser(ctx, name, hash, *isAdmin); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("user %q already exists", name)
			}
			return err
		}
		fmt.Printf("created user %s (admin=%v)\n", name, *isAdmin)
	case "passwd":
		u, err := lookup(ctx, st, name)
		if err != nil {
			return err
		}
		pw, err := readNewPassword()
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := st.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		n, err := st.DeleteOtherSessions(ctx, u.ID, "")
		if err != nil {
			return err
		}
		fmt.Printf("password updated for %s; signed out %d session(s)\n", u.Username, len(n))
	case "admin":
		u, err := lookup(ctx, st, name)
		if err != nil {
			return err
		}
		if len(positional) != 2 || (positional[1] != "on" && positional[1] != "off") {
			return errors.New("usage: hopclip user admin NAME on|off")
		}
		if err := st.SetAdmin(ctx, u.ID, positional[1] == "on"); err != nil {
			return err
		}
		fmt.Printf("%s admin=%s\n", u.Username, positional[1])
	case "del":
		u, err := lookup(ctx, st, name)
		if err != nil {
			return err
		}
		if err := st.DeleteUser(ctx, u.ID); err != nil {
			return err
		}
		fmt.Printf("deleted user %s and all their clips, files and sessions\n", u.Username)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown user subcommand %q", sub)
	}
	return nil
}

func lookup(ctx context.Context, st *store.Store, name string) (*store.User, error) {
	u, err := st.UserByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no such user %q", name)
	}
	return u, err
}

func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on stdin")
		}
		pw := strings.TrimRight(line, "\r\n")
		return pw, auth.ValidatePassword(pw)
	}
	fmt.Fprint(os.Stderr, "New password: ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if err := auth.ValidatePassword(string(a)); err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords do not match")
	}
	return string(a), nil
}

func healthcheck() error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}

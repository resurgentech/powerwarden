// Command powermaster is the lab host power daemon (`serve`) and its
// operator TUI (`monitor`). `check` validates a config against the Vault
// inventory and exits non-zero on any error. See docs/DESIGN-powermaster.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/resurgentech/keyclient/keylist"
	powermaster "github.com/resurgentech/powerwarden/powermaster/daemon"
	"github.com/resurgentech/powerwarden/powermaster/daemon/hw"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/events"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/httpapi"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/keysource"
	"github.com/resurgentech/powerwarden/powermaster/daemon/internal/tui"
	"github.com/resurgentech/powerwarden/powermaster/daemon/inventory"
	"github.com/resurgentech/powerwarden/powermaster/daemon/store"
)

const defaultConfig = "/etc/powermaster/config.yaml"

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  powermaster serve   [--config %s]
  powermaster check   [--config %s]
  powermaster monitor [--config FILE] [--api-url URL] [--token-file FILE]
`, defaultConfig, defaultConfig)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "check":
		err = check(ctx, os.Args[2:])
	case "monitor":
		err = monitor(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "powermaster:", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// vaultInventory loads the inventory from Vault as serve does.
func vaultInventory(log *slog.Logger) powermaster.InventoryLoader {
	return func(ctx context.Context, cfg powermaster.Config) (*inventory.Inventory, error) {
		return inventory.Vault{
			AddrEnv: cfg.Vault.AddrEnv, TokenEnv: cfg.Vault.TokenEnv, SkipVerifyEnv: cfg.Vault.SkipVerifyEnv,
			SecretPath: cfg.Vault.HostsSecretPath, Log: log,
		}.Load(ctx)
	}
}

// check loads the config and the Vault inventory and runs Resolve.
func check(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "config file")
	_ = fs.Parse(args)
	n, err := powermaster.Check(ctx, *cfgPath, vaultInventory(newLogger("warn")))
	if err != nil {
		return err
	}
	// Check validated the config, so loading it again cannot fail on content.
	cfg, err := powermaster.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	keys, err := keyListSuffix(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %s resolves against the inventory (%d hosts)%s%s\n", *cfgPath, n, cfg.CheckSuffix(), keys)
	return nil
}

// buildKeys builds the pulled key list with the helper and options serve and
// check share. Built once, after the tokens are loaded: tokens.yaml is not
// reloaded while running, so the Source's Accept stays stable for the life of
// the process. It returns nil when personal_keys.url is unset.
func buildKeys(cfg powermaster.Config, tokens *httpapi.Tokens, log *slog.Logger) (*keylist.Source, error) {
	return keysource.New(cfg.PersonalKeys, httpapi.PulledRoles, tokens.TakenLocally, log)
}

// startKeys starts the key list with ctx and logs its one line. serve calls it
// after the handler is built and before the listener starts; nil does nothing.
func startKeys(ctx context.Context, src *keylist.Source, log *slog.Logger) error {
	return keysource.Start(ctx, src, log)
}

// keyListSuffix is check's part for the key list. With personal_keys.url unset
// it is empty and loads nothing. Otherwise it loads the tokens file with serve's
// loader (Accept needs it) and builds the source exactly as serve does, never
// starting it: nothing is sent and no cache file is written. The suffix is
// " key_list=on", then " key_list_insecure_http=on" when allow_insecure_http is
// true and the URL is plain http.
func keyListSuffix(cfg powermaster.Config) (string, error) {
	if cfg.PersonalKeys.URL == "" {
		return "", nil
	}
	tokens, err := httpapi.LoadTokens(cfg.TokensFile)
	if err != nil {
		return "", err
	}
	if _, err := buildKeys(cfg, tokens, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		return "", err
	}
	suffix := " key_list=on"
	if u, err := url.Parse(cfg.PersonalKeys.URL); err == nil && u.Scheme == "http" && cfg.PersonalKeys.AllowInsecureHTTP {
		suffix += " key_list_insecure_http=on"
	}
	return suffix, nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "config file")
	_ = fs.Parse(args)

	cfg, err := powermaster.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateInventory(); err != nil {
		return fmt.Errorf("%s: %w", *cfgPath, err)
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	inv, err := vaultInventory(log)(ctx, cfg)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	tokens, err := httpapi.LoadTokens(cfg.TokensFile)
	if err != nil {
		return err
	}
	keys, err := buildKeys(cfg, tokens, log)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer st.Close()

	level := 2
	if cfg.LogLevel == "debug" {
		level = 3
	}
	mgr, err := powermaster.New(powermaster.Options{
		Config: cfg, Inventory: inv, Store: st, HW: hw.Real{Log: log, LogLevel: level},
		Hub: events.NewHub(cfg.Events.RingSize), Log: log,
	})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := mgr.Start(runCtx); err != nil {
		return err
	}
	api, err := httpapi.New(httpapi.Options{Manager: mgr, Tokens: tokens, Log: log, Heartbeat: cfg.Events.Heartbeat.D(),
		KeyRequestURL: cfg.PersonalKeys.RequestURL, Keys: keys})
	if err != nil {
		return err
	}
	if err := startKeys(ctx, keys, log); err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: api, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("powermaster serving", "listen", cfg.Listen, "hosts", len(inv.Hosts), "state_path", cfg.StatePath)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
	cancel()
	mgr.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func monitor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (optional; tui section)")
	apiURL := fs.String("api-url", "", "powermaster API base URL (overrides tui.api_url, $POWERMASTER_URL and ~/.config/powermaster/url)")
	tokenFile := fs.String("token-file", "", "file holding the bearer token (overrides tui.token_file)")
	_ = fs.Parse(args)

	cfg := powermaster.DefaultConfig()
	if *cfgPath != "" {
		c, err := powermaster.LoadConfig(*cfgPath)
		if err != nil {
			return err
		}
		cfg = c
	}
	if *tokenFile != "" {
		cfg.TUI.TokenFile = *tokenFile
	}
	home, _ := os.UserHomeDir()
	baseURL, err := powermaster.ResolveMonitorURL(*apiURL, cfg.TUI.APIURL, os.Getenv, home)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(expandHome(cfg.TUI.TokenFile))
	if err != nil {
		return fmt.Errorf("token file: %w", err)
	}
	client, err := tui.NewHTTPClient(baseURL, strings.TrimSpace(string(raw)))
	if err != nil {
		return err
	}
	return tui.Run(ctx, client, tui.Config{
		LockedTaps: cfg.TUI.LockedTaps, LockedTapGap: cfg.TUI.LockedTapGap.D(), LockedForceDelay: cfg.TUI.LockedForceDelay.D(),
	})
}

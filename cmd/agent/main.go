// Command agent is the desktop device agent. It discovers Android and iOS
// devices over USB and exposes them to a web server through an outbound
// WebSocket tunnel and a local HTTP API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ababank/mobile-device-agent/internal/agent"
	"github.com/ababank/mobile-device-agent/internal/config"
)

var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "", "config file (default "+config.DefaultPath()+")")
		serverURL   = flag.String("server", "", "web server tunnel URL, e.g. wss://hub.example.com/agent")
		serverToken = flag.String("server-token", "", "token presented to the web server")
		listen      = flag.String("listen", "", "local API listen address (default 127.0.0.1:7100)")
		name        = flag.String("name", "", "agent display name (default hostname)")
		logLevel    = flag.String("log-level", "", "debug | info | warn | error")
		initCfg     = flag.Bool("init", false, "write an example config file and exit")
		showToken   = flag.Bool("show-token", false, "print the local API token and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *initCfg {
		if err := writeExample(*configPath); err != nil {
			fatal(err)
		}
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}
	override(&cfg.ServerURL, *serverURL)
	override(&cfg.ServerToken, *serverToken)
	override(&cfg.ListenAddr, *listen)
	override(&cfg.AgentName, *name)
	override(&cfg.LogLevel, *logLevel)
	if err := cfg.Validate(); err != nil {
		fatal(err)
	}

	if *showToken {
		tok := cfg.APIToken
		if tok == "" {
			if tok, err = config.EnsureSecret(cfg.DataDir, "api-token", 24); err != nil {
				fatal(err)
			}
		}
		fmt.Println(tok)
		return
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	log.Info("mobile device agent starting", "version", version, "name", cfg.AgentName, "dataDir", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := agent.New(ctx, cfg, log, version)
	if err != nil {
		fatal(err)
	}
	if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
	log.Info("agent stopped")
}

func override(dst *string, v string) {
	if strings.TrimSpace(v) != "" {
		*dst = v
	}
}

func writeExample(path string) error {
	if path == "" {
		path = config.DefaultPath()
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; not overwriting", path)
	}
	b, _ := json.MarshalIndent(config.Example(), "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

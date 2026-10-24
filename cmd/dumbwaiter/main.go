// Command dumbwaiter serves static sites from S3 buckets.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/proxy"
	"github.com/jtprogru/dumbwaiter/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dumbwaiter:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/dumbwaiter/config.yaml", "path to the YAML config")
	check := flag.Bool("check", false, "validate the config and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *check {
		fmt.Println("config ok")
		return nil
	}

	log := newLogger(cfg.Log)
	slog.SetDefault(log)

	sites, err := proxy.New(cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, sites, log)
	if err != nil {
		return err
	}

	log.Info("starting dumbwaiter", "version", version, "hosts", cfg.Hosts())
	return srv.Run(ctx)
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

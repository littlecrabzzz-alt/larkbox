package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "sandbox" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runSandbox(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	}
	defaults := config.Default()
	addr := flag.String("addr", defaults.Addr, "HTTP listen address")
	path := flag.String("config", "", "path to a YAML or JSON configuration file")
	strict := flag.Bool("strict", defaults.StrictMode, "enable strict Feishu protocol validation")
	version := flag.Bool("version", false, "print the version and exit")
	noUI := flag.Bool("no-ui", false, "disable the embedded Web Inspector")
	readyFile := flag.String("ready-file", "", "write the bound address to this file at startup")
	flag.Usage = func() {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "Usage: larkbox [server flags] or larkbox sandbox <command> [flags]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *version {
		fmt.Println(server.Version)
		return nil
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.Addr = *addr
		case "strict":
			cfg.StrictMode = *strict
		}
	})
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, *noUI, *readyFile)
}

func serve(ctx context.Context, cfg config.Config, noUI bool, readyFile string) error {
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	defer func() { _ = listener.Close() }()
	var opts []server.Option
	if noUI {
		opts = append(opts, server.WithoutWebUI())
	}
	var logger *slog.Logger
	if cfg.JSONLogs {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
		opts = append(opts, server.WithLogger(logger))
	}
	srv := server.New(cfg, opts...)
	if err := srv.LoadCheckpoint(); err != nil {
		return err
	}
	if readyFile != "" {
		if err := writeReadyFile(readyFile, listener.Addr().String()); err != nil {
			return fmt.Errorf("write ready file: %w", err)
		}
	}
	if logger != nil {
		logger.Info("server_ready", "version", server.Version, "address", listener.Addr().String(),
			"strict_mode", cfg.StrictMode, "sandbox_auth", cfg.SandboxToken != "", "inspector", !noUI)
	} else {
		log.Printf("larkbox %s listening on %s (strict mode: %t)", server.Version, listener.Addr(), cfg.StrictMode)
		if !noUI {
			address := clientAddress(listener.Addr().String())
			log.Printf("Web UI: %s", (&url.URL{Scheme: "http", Host: address, Path: "/"}).String())
		}
	}

	go srv.CheckpointUntil(ctx, func(err error) {
		if logger != nil {
			logger.Error("checkpoint_failed", "error", err.Error())
		} else {
			log.Printf("checkpoint failed: %v", err)
		}
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		err = <-done
	case err = <-done:
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	return nil
}

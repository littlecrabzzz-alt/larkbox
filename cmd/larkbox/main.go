package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	srv := server.New(cfg, opts...)
	if readyFile != "" {
		if err := writeReadyFile(readyFile, listener.Addr().String()); err != nil {
			return fmt.Errorf("write ready file: %w", err)
		}
	}
	log.Printf("larkbox %s listening on %s (strict mode: %t)", server.Version, listener.Addr(), cfg.StrictMode)
	if !noUI {
		address := clientAddress(listener.Addr().String())
		log.Printf("Web UI: %s", (&url.URL{Scheme: "http", Host: address, Path: "/"}).String())
	}
	fixture := config.DefaultApp()
	if app, ok := srv.Store().Apps().Get(fixture.AppID); ok && app.AppSecret == fixture.AppSecret {
		log.Printf("Local app credentials: app_id=%s app_secret=%s", app.AppID, app.AppSecret)
	}

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

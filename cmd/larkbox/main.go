package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
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
	defaults := config.Default()
	addr := flag.String("addr", defaults.Addr, "HTTP listen address")
	path := flag.String("config", "", "path to a YAML or JSON configuration file")
	strict := flag.Bool("strict", defaults.StrictMode, "enable strict Feishu protocol validation")
	version := flag.Bool("version", false, "print the version and exit")
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
	return serve(ctx, cfg)
}

func serve(ctx context.Context, cfg config.Config) error {
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	srv := server.New(cfg)
	log.Printf("larkbox %s listening on %s (strict mode: %t)", server.Version, listener.Addr(), cfg.StrictMode)
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

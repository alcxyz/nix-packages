// Command deck serves a live dashboard of T3 Code threads. It reads facts
// from the T3 adapter commands, keeps a summary per thread made by the
// configured model roles, and checks the OIDC ID token its proxy forwards.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  deck serve [-config PATH] [-dev-no-auth]
  deck version`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "deck:", err)
			os.Exit(1)
		}
	case "version", "-version", "--version":
		fmt.Println("deck", version)
	case "help", "-h", "-help", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "configuration file")
	devNoAuth := fs.Bool("dev-no-auth", false, "disable the token check (loopback listen address only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.validateAuth(*devNoAuth); err != nil {
		return err
	}

	cachePath, err := defaultCachePath()
	if err != nil {
		return err
	}
	cache, err := openSummaryCache(cachePath)
	if err != nil {
		return err
	}

	// A missing or broken default role leaves facts and other roles working.
	llmPath, roleErr := sharedConfigPath()
	var models map[string][]ModelConfig
	if roleErr == nil {
		models, roleErr = loadSummaryRoles(llmPath, cfg.Threads, log)
	}
	if roleErr != nil {
		log.Warn("automatic summaries disabled", "err", roleErr.Error())
	}

	var auth *Authenticator
	if *devNoAuth {
		log.Warn("token check disabled (-dev-no-auth)", "listen", cfg.Listen)
	} else {
		auth = newAuthenticator(cfg.Auth, &http.Client{Timeout: 15 * time.Second}, log)
	}

	d := newDeck(cfg, cache, models, roleErr, log)
	if d.backlog != nil && cfg.Backlog.Commands.Item != "" {
		path, err := defaultVerdictPath()
		if err != nil {
			return err
		}
		if d.backlog.cache, err = openVerdictCache(path); err != nil {
			return err
		}
	}
	handler, err := newHandler(d, auth, log)
	if err != nil {
		return err
	}

	// Bind before starting any background work, so a failure to listen
	// leaves no command running.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d.run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "basePath", cfg.BasePath, "version", version)
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		// Serving failed: stop the background work and wait for it, as on
		// shutdown.
		stop()
		if !d.wait(10 * time.Second) {
			log.Warn("background work did not stop in time")
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// The cancelled context stops the background work and any action, and
	// kills their commands; wait for that before exiting, so none is left
	// running.
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	if !d.wait(10 * time.Second) {
		log.Warn("background work did not stop in time")
	}
	return err
}

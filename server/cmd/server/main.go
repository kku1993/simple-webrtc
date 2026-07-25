// Package main is the simple-peer signaling server entry point.
//
// It loads configuration from the environment, validates startup invariants
// (refusing to start without a server secret or allowed origins), wires the
// room registry to the WebSocket server, and runs until SIGINT/SIGTERM.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kku1993/simple-peer-signal-server/internal/config"
	"github.com/kku1993/simple-peer-signal-server/internal/metrics"
	"github.com/kku1993/simple-peer-signal-server/internal/room"
	"github.com/kku1993/simple-peer-signal-server/internal/server"
	"github.com/kku1993/simple-peer-signal-server/internal/token"
	"github.com/kku1993/simple-peer-signal-server/internal/tombstone"
	"github.com/kku1993/simple-peer-signal-server/internal/turnstile"
	"github.com/kku1993/simple-peer-signal-server/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if cfg.OriginsCheckDisabled() {
		log.Printf("WARNING: ALLOWED_ORIGINS is \"*\"; origin checking is disabled")
	}

	signer, err := token.NewSigner(cfg.ServerSecret)
	if err != nil {
		return err
	}

	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())
	reg := room.New(cfg, signer, tomb, m)
	reg.StartSweep()
	defer reg.Stop()

	var ts *turnstile.Client
	if cfg.TurnstileSecretKey != "" {
		ts = turnstile.New(cfg.TurnstileSecretKey)
	}

	srv := server.New(cfg, reg, m, ts)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case sig := <-sigCh:
		log.Printf("received %s; draining", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	return nil
}

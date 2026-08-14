// Package main is the simple-peer signaling server entry point.
//
// It loads configuration from the environment, validates startup invariants
// (refusing to start without a server secret or allowed origins), wires the
// room registry to the WebSocket server, and runs until SIGINT/SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/config"
	"github.com/kku1993/simple-webrtc-server/internal/metrics"
	"github.com/kku1993/simple-webrtc-server/internal/requestlog"
	"github.com/kku1993/simple-webrtc-server/internal/room"
	"github.com/kku1993/simple-webrtc-server/internal/server"
	"github.com/kku1993/simple-webrtc-server/internal/token"
	"github.com/kku1993/simple-webrtc-server/internal/tombstone"
	"github.com/kku1993/simple-webrtc-server/internal/turn"
	"github.com/kku1993/simple-webrtc-server/internal/turnstile"
	"github.com/kku1993/simple-webrtc-server/internal/version"
)

func main() {
	// --version prints the stamped version and exits. Handled before any other
	// startup work so it works without SERVER_SECRET/ALLOWED_ORIGINS set.
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log.Printf("simple-webrtc-server %s starting on %s", version.Version, cfg.ListenAddr)

	if cfg.OriginsCheckDisabled() {
		log.Printf("WARNING: ALLOWED_ORIGINS is \"*\"; origin checking is disabled")
	}

	signer, err := token.NewSigner(cfg.ServerSecret)
	if err != nil {
		return err
	}

	m := metrics.New()
	tomb := tombstone.New(cfg.TombstoneMaxEntries, cfg.TombstoneTtl())

	var turnClient *turn.Client
	if cfg.TurnEnabled() {
		turnClient = turn.New(cfg.TurnKeyID, cfg.TurnKeyAPIToken, cfg.TurnCredentialTtl())
		log.Printf("TURN enabled; minting %s credentials via Cloudflare Calls", cfg.TurnCredentialTtl())
	}

	reg := room.New(cfg, signer, tomb, m, turnClient)
	reg.StartSweep()
	defer reg.Stop()

	var ts *turnstile.Client
	if cfg.TurnstileSecretKey != "" {
		ts = turnstile.New(cfg.TurnstileSecretKey)
	}

	srv := server.New(cfg, reg, m, ts, requestlog.New(os.Stdout))

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

// Package main is the simple-peer signaling server entry point.
//
// It loads configuration from the environment (overridable with command-line
// flags), validates startup invariants (refusing to start without a server
// secret or allowed origins), wires the room registry to the WebSocket server,
// and runs until SIGINT/SIGTERM.
//
// Every setting has both an environment variable and a command-line flag. A
// flag that is explicitly set on the command line takes precedence over its
// env-var counterpart; an unset flag falls back to the env var, and then to the
// built-in default. See the per-flag usage text for the matching env var.
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
	overrides, showVersion := registerFlags()
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	if err := run(overrides()); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// registerFlags defines a command-line flag for every configurable setting and
// returns a closure that builds a config.FlagOverrides populated with pointers
// only for the flags the operator actually passed. The closure must be called
// after flag.Parse so flag.Visit reflects the parsed set.
//
// Each flag's usage text names the env var it overrides so `--help` doubles as
// env-var documentation. Flags default to the empty/zero value; because the
// closure only emits pointers for flags that were explicitly set, an unset flag
// never clobbers the env var / default resolution in config.LoadWithOverrides.
func registerFlags() (buildOverrides func() config.FlagOverrides, showVersion *bool) {
	showVersion = flag.Bool("version", false, "print version and exit")

	// envVar annotates a flag's usage with the env var it overrides.
	envVar := func(env, detail string) string {
		if detail == "" {
			return fmt.Sprintf("overrides %s", env)
		}
		return fmt.Sprintf("%s (overrides %s)", detail, env)
	}

	fListenAddr := flag.String("listen-addr", "", envVar("LISTEN_ADDR", "TCP address to listen on"))
	fServerSecret := flag.String("server-secret", "", envVar("SERVER_SECRET", "HMAC signing secret, >=32 bytes (required)"))
	fAllowedOrigins := flag.String("allowed-origins", "", envVar("ALLOWED_ORIGINS", "comma-separated origins; \"*\" disables the check (required)"))
	fShardName := flag.String("shard-name", "", envVar("SHARD_NAME", "single alphabetic Crockford base32 char (required)"))
	fTrustedProxyCount := flag.Int("trusted-proxy-count", 0, envVar("TRUSTED_PROXY_COUNT", "number of trusted reverse proxies in front of the server"))
	fCloudflareMode := flag.Bool("cloudflare-mode", false, envVar("CLOUDFLARE_MODE", "prefer CF-Connecting-IP for client IPs"))
	fTurnstileSecretKey := flag.String("turnstile-secret-key", "", envVar("TURNSTILE_SECRET_KEY", "Cloudflare Turnstile siteverify secret; enables bot prevention on create-room"))
	fTurnKeyID := flag.String("turn-key-id", "", envVar("TURN_KEY_ID", "Cloudflare Calls TURN key id"))
	fTurnKeyAPIToken := flag.String("turn-key-api-token", "", envVar("TURN_KEY_API_TOKEN", "Cloudflare Calls TURN key API token"))
	fTurnCredentialTtlSec := flag.Int("turn-credential-ttl-sec", 0, envVar("TURN_CREDENTIAL_TTL_SEC", "lifetime (seconds) of minted TURN credentials"))
	fPeerDeadlineSec := flag.Int("peer-deadline-sec", 0, envVar("PEER_DEADLINE_SEC", "peer connection deadline for rooms with one empty slot"))
	fRoomMaxLifetimeSec := flag.Int("room-max-lifetime-sec", 0, envVar("ROOM_MAX_LIFETIME_SEC", "in-memory room max lifetime"))
	fRejoinTokenTtlSec := flag.Int("rejoin-token-ttl-sec", 0, envVar("REJOIN_TOKEN_TTL_SEC", "rejoin token TTL"))
	fReleaseSocketsOnPeerConnected := flag.Bool("release-sockets-on-peer-connected", false, envVar("RELEASE_SOCKETS_ON_PEER_CONNECTED", "release server sockets once peers connect"))
	fPeerConnectedGraceSec := flag.Int("peer-connected-grace-sec", 0, envVar("PEER_CONNECTED_GRACE_SEC", "grace period before sockets are released"))
	fMaxFrameBytes := flag.Int("max-frame-bytes", 0, envVar("MAX_FRAME_BYTES", "maximum WebSocket frame size in bytes"))
	fMaxBufferedSignals := flag.Int("max-buffered-signals", 0, envVar("MAX_BUFFERED_SIGNALS", "max signals buffered per slot"))
	fMaxBufferedSignalBytes := flag.Int("max-buffered-signal-bytes", 0, envVar("MAX_BUFFERED_SIGNAL_BYTES", "max signal bytes buffered per slot"))
	fMaxPasswordAttempts := flag.Int("max-password-attempts", 0, envVar("MAX_PASSWORD_ATTEMPTS", "max password attempts per room"))
	fHandshakeTimeoutMs := flag.Int("handshake-timeout-ms", 0, envVar("HANDSHAKE_TIMEOUT_MS", "deadline for the first client message, in milliseconds"))
	fPingIntervalSec := flag.Int("ping-interval-sec", 0, envVar("PING_INTERVAL_SEC", "WebSocket ping interval; close after 2 missed pongs"))
	fTombstoneMaxEntries := flag.Int("tombstone-max-entries", 0, envVar("TOMBSTONE_MAX_ENTRIES", "bounded LRU size for closed-room tombstones"))
	fTombstoneTtlSec := flag.Int("tombstone-ttl-sec", 0, envVar("TOMBSTONE_TTL_SEC", "tombstone TTL"))
	fMaxRoomsGlobal := flag.Int("max-rooms-global", 0, envVar("MAX_ROOMS_GLOBAL", "global cap on concurrent rooms"))
	fMaxConnectionsGlobal := flag.Int("max-connections-global", 0, envVar("MAX_CONNECTIONS_GLOBAL", "global cap on concurrent WebSocket connections"))
	fMaxRoomsPerIp := flag.Int("max-rooms-per-ip", 0, envVar("MAX_ROOMS_PER_IP", "concurrent rooms per client IP"))

	// flag.Visit only iterates flags that were explicitly set, so the closure
	// emits pointers solely for those — preserving env-var/default fallback for
	// the rest.
	buildOverrides = func() config.FlagOverrides {
		set := map[string]struct{}{}
		flag.Visit(func(f *flag.Flag) { set[f.Name] = struct{}{} })

		o := config.FlagOverrides{}
		if _, ok := set["listen-addr"]; ok {
			o.ListenAddr = fListenAddr
		}
		if _, ok := set["server-secret"]; ok {
			o.ServerSecret = fServerSecret
		}
		if _, ok := set["allowed-origins"]; ok {
			o.AllowedOrigins = fAllowedOrigins
		}
		if _, ok := set["shard-name"]; ok {
			o.ShardName = fShardName
		}
		if _, ok := set["trusted-proxy-count"]; ok {
			o.TrustedProxyCount = fTrustedProxyCount
		}
		if _, ok := set["cloudflare-mode"]; ok {
			o.CloudflareMode = fCloudflareMode
		}
		if _, ok := set["turnstile-secret-key"]; ok {
			o.TurnstileSecretKey = fTurnstileSecretKey
		}
		if _, ok := set["turn-key-id"]; ok {
			o.TurnKeyID = fTurnKeyID
		}
		if _, ok := set["turn-key-api-token"]; ok {
			o.TurnKeyAPIToken = fTurnKeyAPIToken
		}
		if _, ok := set["turn-credential-ttl-sec"]; ok {
			o.TurnCredentialTtlSec = fTurnCredentialTtlSec
		}
		if _, ok := set["peer-deadline-sec"]; ok {
			o.PeerDeadlineSec = fPeerDeadlineSec
		}
		if _, ok := set["room-max-lifetime-sec"]; ok {
			o.RoomMaxLifetimeSec = fRoomMaxLifetimeSec
		}
		if _, ok := set["rejoin-token-ttl-sec"]; ok {
			o.RejoinTokenTtlSec = fRejoinTokenTtlSec
		}
		if _, ok := set["release-sockets-on-peer-connected"]; ok {
			o.ReleaseSocketsOnPeerConnected = fReleaseSocketsOnPeerConnected
		}
		if _, ok := set["peer-connected-grace-sec"]; ok {
			o.PeerConnectedGraceSec = fPeerConnectedGraceSec
		}
		if _, ok := set["max-frame-bytes"]; ok {
			o.MaxFrameBytes = fMaxFrameBytes
		}
		if _, ok := set["max-buffered-signals"]; ok {
			o.MaxBufferedSignals = fMaxBufferedSignals
		}
		if _, ok := set["max-buffered-signal-bytes"]; ok {
			o.MaxBufferedSignalBytes = fMaxBufferedSignalBytes
		}
		if _, ok := set["max-password-attempts"]; ok {
			o.MaxPasswordAttempts = fMaxPasswordAttempts
		}
		if _, ok := set["handshake-timeout-ms"]; ok {
			o.HandshakeTimeoutMs = fHandshakeTimeoutMs
		}
		if _, ok := set["ping-interval-sec"]; ok {
			o.PingIntervalSec = fPingIntervalSec
		}
		if _, ok := set["tombstone-max-entries"]; ok {
			o.TombstoneMaxEntries = fTombstoneMaxEntries
		}
		if _, ok := set["tombstone-ttl-sec"]; ok {
			o.TombstoneTtlSec = fTombstoneTtlSec
		}
		if _, ok := set["max-rooms-global"]; ok {
			o.MaxRoomsGlobal = fMaxRoomsGlobal
		}
		if _, ok := set["max-connections-global"]; ok {
			o.MaxConnectionsGlobal = fMaxConnectionsGlobal
		}
		if _, ok := set["max-rooms-per-ip"]; ok {
			o.MaxRoomsPerIp = fMaxRoomsPerIp
		}
		return o
	}
	return buildOverrides, showVersion
}

func run(overrides config.FlagOverrides) error {
	cfg, err := config.LoadWithOverrides(overrides)
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

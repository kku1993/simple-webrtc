// Package config holds all runtime configuration for the signaling server.
//
// Every field maps 1:1 to an entry in the "Configuration reference" table of
// docs/DESIGN.md. Values are sourced from environment variables; defaults match
// the design doc.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kku1993/simple-webrtc-server/internal/roomid"
)

// Config is the validated server configuration.
type Config struct {
	ListenAddr                  string
	ServerSecret                []byte
	AllowedOrigins              []string // "*" disables the check
	ShardName                   string   // single alphabetic Crockford base32 char
	TrustedProxyCount           int
	CloudflareMode              bool
	TurnstileSecretKey          string
	PeerDeadlineSec             int
	RoomMaxLifetimeSec          int
	RejoinTokenTtlSec           int
	ReleaseSocketsOnPeerConnected bool
	PeerConnectedGraceSec       int
	MaxFrameBytes               int
	MaxBufferedSignals          int
	MaxBufferedSignalBytes      int
	MaxPasswordAttempts         int
	HandshakeTimeoutMs          int
	PingIntervalSec             int
	TombstoneMaxEntries         int
	TombstoneTtlSec             int
	MaxRoomsGlobal              int
	MaxConnectionsGlobal        int
	MaxRoomsPerIp               int
}

// Load reads configuration from the process environment, applies defaults, and
// validates the result. It returns a non-nil error if the configuration is
// invalid or incomplete in a way that should prevent startup.
func Load() (Config, error) {
	c := Config{
		ListenAddr:                    ":8080",
		PeerDeadlineSec:               600,
		RoomMaxLifetimeSec:            5400,
		RejoinTokenTtlSec:             43200,
		ReleaseSocketsOnPeerConnected: true,
		PeerConnectedGraceSec:         60,
		MaxFrameBytes:                 65536,
		MaxBufferedSignals:            64,
		MaxBufferedSignalBytes:        262144,
		MaxPasswordAttempts:           5,
		HandshakeTimeoutMs:            10000,
		PingIntervalSec:               30,
		TombstoneMaxEntries:           100000,
		TombstoneTtlSec:               3600,
		MaxRoomsGlobal:                50000,
		MaxConnectionsGlobal:          100000,
		MaxRoomsPerIp:                 20,
	}

	get := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	getInt := func(key string, dst *int) error {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = n
		}
		return nil
	}
	getBool := func(key string, dst *bool) error {
		if v, ok := os.LookupEnv(key); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				*dst = true
			case "0", "false", "no", "off":
				*dst = false
			default:
				return fmt.Errorf("%s: invalid boolean %q", key, v)
			}
		}
		return nil
	}

	get("LISTEN_ADDR", &c.ListenAddr)
	if v, ok := os.LookupEnv("SERVER_SECRET"); ok {
		c.ServerSecret = []byte(v)
	}
	if v, ok := os.LookupEnv("ALLOWED_ORIGINS"); ok {
		c.AllowedOrigins = parseOrigins(v)
	}
	get("SHARD_NAME", &c.ShardName)
	if err := getInt("TRUSTED_PROXY_COUNT", &c.TrustedProxyCount); err != nil {
		return Config{}, err
	}
	if err := getBool("CLOUDFLARE_MODE", &c.CloudflareMode); err != nil {
		return Config{}, err
	}
	get("TURNSTILE_SECRET_KEY", &c.TurnstileSecretKey)
	if err := getInt("PEER_DEADLINE_SEC", &c.PeerDeadlineSec); err != nil {
		return Config{}, err
	}
	if err := getInt("ROOM_MAX_LIFETIME_SEC", &c.RoomMaxLifetimeSec); err != nil {
		return Config{}, err
	}
	if err := getInt("REJOIN_TOKEN_TTL_SEC", &c.RejoinTokenTtlSec); err != nil {
		return Config{}, err
	}
	if err := getBool("RELEASE_SOCKETS_ON_PEER_CONNECTED", &c.ReleaseSocketsOnPeerConnected); err != nil {
		return Config{}, err
	}
	if err := getInt("PEER_CONNECTED_GRACE_SEC", &c.PeerConnectedGraceSec); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_FRAME_BYTES", &c.MaxFrameBytes); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_BUFFERED_SIGNALS", &c.MaxBufferedSignals); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_BUFFERED_SIGNAL_BYTES", &c.MaxBufferedSignalBytes); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_PASSWORD_ATTEMPTS", &c.MaxPasswordAttempts); err != nil {
		return Config{}, err
	}
	if err := getInt("HANDSHAKE_TIMEOUT_MS", &c.HandshakeTimeoutMs); err != nil {
		return Config{}, err
	}
	if err := getInt("PING_INTERVAL_SEC", &c.PingIntervalSec); err != nil {
		return Config{}, err
	}
	if err := getInt("TOMBSTONE_MAX_ENTRIES", &c.TombstoneMaxEntries); err != nil {
		return Config{}, err
	}
	if err := getInt("TOMBSTONE_TTL_SEC", &c.TombstoneTtlSec); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_ROOMS_GLOBAL", &c.MaxRoomsGlobal); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_CONNECTIONS_GLOBAL", &c.MaxConnectionsGlobal); err != nil {
		return Config{}, err
	}
	if err := getInt("MAX_ROOMS_PER_IP", &c.MaxRoomsPerIp); err != nil {
		return Config{}, err
	}

	return c, c.Validate()
}

func parseOrigins(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Validate enforces the startup invariants described in the design doc.
func (c Config) Validate() error {
	if len(c.ServerSecret) < 32 {
		return errors.New("SERVER_SECRET must be set to at least 32 bytes")
	}
	if len(c.AllowedOrigins) == 0 {
		return errors.New("ALLOWED_ORIGINS must be set; use \"*\" to disable origin checking")
	}
	if !roomid.IsValidShardName(c.ShardName) {
		return errors.New("SHARD_NAME must be set to a single alphabetic Crockford base32 character (a-z excluding i, l, o, u)")
	}
	if c.MaxFrameBytes <= 0 {
		return errors.New("MAX_FRAME_BYTES must be positive")
	}
	if c.MaxBufferedSignals <= 0 || c.MaxBufferedSignalBytes <= 0 {
		return errors.New("buffer limits must be positive")
	}
	if c.MaxPasswordAttempts <= 0 {
		return errors.New("MAX_PASSWORD_ATTEMPTS must be positive")
	}
	if c.HandshakeTimeoutMs <= 0 {
		return errors.New("HANDSHAKE_TIMEOUT_MS must be positive")
	}
	if c.PingIntervalSec <= 0 {
		return errors.New("PING_INTERVAL_SEC must be positive")
	}
	if c.TombstoneMaxEntries <= 0 {
		return errors.New("TOMBSTONE_MAX_ENTRIES must be positive")
	}
	if c.PeerDeadlineSec <= 0 || c.RoomMaxLifetimeSec <= 0 || c.RejoinTokenTtlSec <= 0 {
		return errors.New("lifetime configs must be positive")
	}
	if c.MaxRoomsGlobal <= 0 || c.MaxConnectionsGlobal <= 0 || c.MaxRoomsPerIp <= 0 {
		return errors.New("capacity configs must be positive")
	}
	return nil
}

// PeerDeadline returns the configured peer deadline as a Duration.
func (c Config) PeerDeadline() time.Duration { return time.Duration(c.PeerDeadlineSec) * time.Second }

// RoomMaxLifetime returns the configured in-memory room lifetime.
func (c Config) RoomMaxLifetime() time.Duration {
	return time.Duration(c.RoomMaxLifetimeSec) * time.Second
}

// RejoinTokenTtl returns the configured rejoin token TTL.
func (c Config) RejoinTokenTtl() time.Duration {
	return time.Duration(c.RejoinTokenTtlSec) * time.Second
}

// PeerConnectedGrace returns the grace period before sockets are released.
func (c Config) PeerConnectedGrace() time.Duration {
	return time.Duration(c.PeerConnectedGraceSec) * time.Second
}

// HandshakeTimeout returns the deadline for the first client message.
func (c Config) HandshakeTimeout() time.Duration {
	return time.Duration(c.HandshakeTimeoutMs) * time.Millisecond
}

// PingInterval returns the WebSocket ping interval.
func (c Config) PingInterval() time.Duration {
	return time.Duration(c.PingIntervalSec) * time.Second
}

// TombstoneTtl returns the tombstone TTL.
func (c Config) TombstoneTtl() time.Duration {
	return time.Duration(c.TombstoneTtlSec) * time.Second
}

// OriginAllowed reports whether the given Origin header is accepted. The check
// is exact string match against each configured entry, except that the single
// entry "*" disables the check entirely.
func (c Config) OriginAllowed(origin string) bool {
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			return true
		}
		if o == origin {
			return true
		}
	}
	return false
}

// OriginsCheckDisabled reports whether the operator explicitly disabled the
// origin check by setting ALLOWED_ORIGINS=*.
func (c Config) OriginsCheckDisabled() bool {
	return len(c.AllowedOrigins) == 1 && c.AllowedOrigins[0] == "*"
}

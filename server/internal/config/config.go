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
	TurnKeyID                   string
	TurnKeyAPIToken             string
	TurnCredentialTtlSec        int
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

	// State persistence. When StateDir is non-empty, room state is
	// periodically flushed to JSON files in that directory so a restarted
	// server can rehydrate live rooms. Empty disables persistence.
	StateDir             string
	StateFlushIntervalMs int
	StateBatchSize       int
}

// FlagOverrides holds optional command-line overrides for every configurable
// field. A nil pointer means "not supplied on the command line"; LoadWithOverrides
// falls back to the corresponding environment variable (and then the built-in
// default) for nil entries. Non-nil entries take precedence over both the
// environment variable and the default — this is how a command-line flag
// overrides its env-var counterpart.
type FlagOverrides struct {
	ListenAddr                    *string
	ServerSecret                  *string
	AllowedOrigins                *string
	ShardName                     *string
	TrustedProxyCount             *int
	CloudflareMode                *bool
	TurnstileSecretKey            *string
	TurnKeyID                     *string
	TurnKeyAPIToken               *string
	TurnCredentialTtlSec          *int
	PeerDeadlineSec               *int
	RoomMaxLifetimeSec            *int
	RejoinTokenTtlSec             *int
	ReleaseSocketsOnPeerConnected *bool
	PeerConnectedGraceSec         *int
	MaxFrameBytes                 *int
	MaxBufferedSignals            *int
	MaxBufferedSignalBytes        *int
	MaxPasswordAttempts           *int
	HandshakeTimeoutMs            *int
	PingIntervalSec               *int
	TombstoneMaxEntries           *int
	TombstoneTtlSec               *int
	MaxRoomsGlobal                *int
	MaxConnectionsGlobal          *int
	MaxRoomsPerIp                 *int
	StateDir                      *string
	StateFlushIntervalMs          *int
	StateBatchSize                *int
}

// Load reads configuration from the process environment, applies defaults, and
// validates the result. It is equivalent to LoadWithOverrides with no overrides
// (every field falls back to env var / default).
func Load() (Config, error) {
	return LoadWithOverrides(FlagOverrides{})
}

// LoadWithOverrides reads configuration from the process environment, then
// applies any non-nil command-line overrides on top, and validates the result.
// A non-nil override wins over both the env var and the default. It returns a
// non-nil error if the configuration is invalid or incomplete in a way that
// should prevent startup.
func LoadWithOverrides(o FlagOverrides) (Config, error) {
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
		TurnCredentialTtlSec:          14400, // 4 hours; matches internal/turn.DefaultTTL
		StateFlushIntervalMs:          100,
		StateBatchSize:                256,
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
	get("TURN_KEY_ID", &c.TurnKeyID)
	get("TURN_KEY_API_TOKEN", &c.TurnKeyAPIToken)
	if err := getInt("TURN_CREDENTIAL_TTL_SEC", &c.TurnCredentialTtlSec); err != nil {
		return Config{}, err
	}
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
	get("STATE_DIR", &c.StateDir)
	if err := getInt("STATE_FLUSH_INTERVAL_MS", &c.StateFlushIntervalMs); err != nil {
		return Config{}, err
	}
	if err := getInt("STATE_BATCH_SIZE", &c.StateBatchSize); err != nil {
		return Config{}, err
	}

	// Command-line overrides win over env vars and defaults. Applied after the
	// env pass so a flag always takes precedence.
	if o.ListenAddr != nil {
		c.ListenAddr = *o.ListenAddr
	}
	if o.ServerSecret != nil {
		c.ServerSecret = []byte(*o.ServerSecret)
	}
	if o.AllowedOrigins != nil {
		c.AllowedOrigins = parseOrigins(*o.AllowedOrigins)
	}
	if o.ShardName != nil {
		c.ShardName = *o.ShardName
	}
	if o.TrustedProxyCount != nil {
		c.TrustedProxyCount = *o.TrustedProxyCount
	}
	if o.CloudflareMode != nil {
		c.CloudflareMode = *o.CloudflareMode
	}
	if o.TurnstileSecretKey != nil {
		c.TurnstileSecretKey = *o.TurnstileSecretKey
	}
	if o.TurnKeyID != nil {
		c.TurnKeyID = *o.TurnKeyID
	}
	if o.TurnKeyAPIToken != nil {
		c.TurnKeyAPIToken = *o.TurnKeyAPIToken
	}
	if o.TurnCredentialTtlSec != nil {
		c.TurnCredentialTtlSec = *o.TurnCredentialTtlSec
	}
	if o.PeerDeadlineSec != nil {
		c.PeerDeadlineSec = *o.PeerDeadlineSec
	}
	if o.RoomMaxLifetimeSec != nil {
		c.RoomMaxLifetimeSec = *o.RoomMaxLifetimeSec
	}
	if o.RejoinTokenTtlSec != nil {
		c.RejoinTokenTtlSec = *o.RejoinTokenTtlSec
	}
	if o.ReleaseSocketsOnPeerConnected != nil {
		c.ReleaseSocketsOnPeerConnected = *o.ReleaseSocketsOnPeerConnected
	}
	if o.PeerConnectedGraceSec != nil {
		c.PeerConnectedGraceSec = *o.PeerConnectedGraceSec
	}
	if o.MaxFrameBytes != nil {
		c.MaxFrameBytes = *o.MaxFrameBytes
	}
	if o.MaxBufferedSignals != nil {
		c.MaxBufferedSignals = *o.MaxBufferedSignals
	}
	if o.MaxBufferedSignalBytes != nil {
		c.MaxBufferedSignalBytes = *o.MaxBufferedSignalBytes
	}
	if o.MaxPasswordAttempts != nil {
		c.MaxPasswordAttempts = *o.MaxPasswordAttempts
	}
	if o.HandshakeTimeoutMs != nil {
		c.HandshakeTimeoutMs = *o.HandshakeTimeoutMs
	}
	if o.PingIntervalSec != nil {
		c.PingIntervalSec = *o.PingIntervalSec
	}
	if o.TombstoneMaxEntries != nil {
		c.TombstoneMaxEntries = *o.TombstoneMaxEntries
	}
	if o.TombstoneTtlSec != nil {
		c.TombstoneTtlSec = *o.TombstoneTtlSec
	}
	if o.MaxRoomsGlobal != nil {
		c.MaxRoomsGlobal = *o.MaxRoomsGlobal
	}
	if o.MaxConnectionsGlobal != nil {
		c.MaxConnectionsGlobal = *o.MaxConnectionsGlobal
	}
	if o.MaxRoomsPerIp != nil {
		c.MaxRoomsPerIp = *o.MaxRoomsPerIp
	}
	if o.StateDir != nil {
		c.StateDir = *o.StateDir
	}
	if o.StateFlushIntervalMs != nil {
		c.StateFlushIntervalMs = *o.StateFlushIntervalMs
	}
	if o.StateBatchSize != nil {
		c.StateBatchSize = *o.StateBatchSize
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
	// TURN credential minting is optional. When enabled, both halves of the
	// Cloudflare Calls TURN key must be supplied: a key id and its API token.
	if (c.TurnKeyID == "") != (c.TurnKeyAPIToken == "") {
		return errors.New("TURN_KEY_ID and TURN_KEY_API_TOKEN must be set together (or both left unset to disable server-provided TURN)")
	}
	if c.TurnKeyID != "" && c.TurnCredentialTtlSec <= 0 {
		return errors.New("TURN_CREDENTIAL_TTL_SEC must be positive when TURN is enabled")
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
	if c.StateDir != "" {
		if c.StateFlushIntervalMs <= 0 {
			return errors.New("STATE_FLUSH_INTERVAL_MS must be positive when STATE_DIR is set")
		}
		if c.StateBatchSize <= 0 {
			return errors.New("STATE_BATCH_SIZE must be positive when STATE_DIR is set")
		}
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

// TurnEnabled reports whether server-provided TURN credentials are configured.
// When false, handshake responses omit the iceServers field.
func (c Config) TurnEnabled() bool {
	return c.TurnKeyID != "" && c.TurnKeyAPIToken != ""
}

// TurnCredentialTtl returns the lifetime of minted TURN credentials.
func (c Config) TurnCredentialTtl() time.Duration {
	return time.Duration(c.TurnCredentialTtlSec) * time.Second
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

// StateEnabled reports whether disk persistence of room state is configured.
func (c Config) StateEnabled() bool { return c.StateDir != "" }

// StateFlushInterval returns the persister flush interval as a Duration.
func (c Config) StateFlushInterval() time.Duration {
	return time.Duration(c.StateFlushIntervalMs) * time.Millisecond
}

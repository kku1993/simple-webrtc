// Package turn mints short-lived TURN/STUN credentials from the Cloudflare
// Calls TURN API.
//
// A TURN key (created in the Cloudflare dashboard or via the API) is a
// long-term secret identified by a key id. This package's Client holds the key
// id and the API token and calls the
// `/v1/turn/keys/{keyId}/credentials/generate-ice-servers` endpoint to mint
// short-lived credentials. The response is an `iceServers` array ready to be
// passed straight to an `RTCPeerConnection` (STUN entries plus a TURN entry
// carrying a username/credential pair), and is relayed to clients in the
// handshake responses — see docs/DESIGN.md §"CreateRoom".
//
// The Client is safe for concurrent use: every call issues a fresh credential.
// There is no caching, because credentials are per-user and short-lived and the
// upstream API imposes no limit on the number minted.
package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DefaultEndpoint is the Cloudflare Calls TURN API root.
const DefaultEndpoint = "https://rtc.live.cloudflare.com"

// DefaultTTL is the credential lifetime used when the caller does not pass one.
// It is long enough for a long video call while keeping the credential
// short-lived relative to a long-term shared secret.
const DefaultTTL = 4 * time.Hour

// IceServer is one entry of the iceServers array returned to the client. It
// mirrors the WebRTC `RTCIceServer` dictionary: a non-empty list of URLs and,
// for TURN servers, a username/credential pair.
//
// The JSON tags match the wire shape Cloudflare returns and the field names a
// browser's RTCPeerConnection expects (lowerCamelCase `urls`, `username`,
// `credential`), so the array can be relayed to the client verbatim.
type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// generateRequest is the body posted to the generate-ice-servers endpoint.
type generateRequest struct {
	TTL              int    `json:"ttl"`
	CustomIdentifier string `json:"customIdentifier,omitempty"`
}

// generateResponse is the shape returned by generate-ice-servers.
type generateResponse struct {
	IceServers []IceServer `json:"iceServers"`
}

// Client mints short-lived TURN credentials from a Cloudflare Calls TURN key.
type Client struct {
	keyID      string
	apiToken   string
	httpClient *http.Client
	endpoint   string
	ttl        time.Duration
}

// New constructs a Client for the given TURN key id and API token.
//
// The API token is the TURN key's token (shown once when the key is created);
// it is sent as a bearer token in the Authorization header and must be kept
// server-side. ttl is the lifetime of each minted credential; pass 0 to use
// DefaultTTL.
func New(keyID, apiToken string, ttl time.Duration) *Client {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Client{
		keyID:      keyID,
		apiToken:   apiToken,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		endpoint:   DefaultEndpoint,
		ttl:        ttl,
	}
}

// SetEndpoint overrides the API root, primarily for tests.
func (c *Client) SetEndpoint(u string) { c.endpoint = u }

// SetHTTPClient overrides the underlying http.Client, primarily for tests.
func (c *Client) SetHTTPClient(h *http.Client) { c.httpClient = h }

// TTL returns the lifetime of each minted credential. Callers use it to size a
// cache that holds a minted iceServers array until the credential would expire.
func (c *Client) TTL() time.Duration { return c.ttl }

// Generate mints a fresh iceServers array. customIdentifier is optional; when
// non-empty it tags the credential in Cloudflare's analytics so usage can be
// aggregated per session. The caller should include a room id and a timestamp
// (e.g. `roomId-unixTimestamp`) so recycled room ids are disambiguated. The
// returned slice is safe to relay directly to a client.
func (c *Client) Generate(ctx context.Context, customIdentifier string) ([]IceServer, error) {
	if c.keyID == "" || c.apiToken == "" {
		return nil, errors.New("turn: key id and api token are required")
	}
	body := generateRequest{
		TTL: int(c.ttl.Seconds()),
	}
	if customIdentifier != "" {
		body.CustomIdentifier = customIdentifier
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("turn: encode request: %w", err)
	}

	url := c.endpoint + "/v1/turn/keys/" + c.keyID + "/credentials/generate-ice-servers"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("turn: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("turn: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("turn: unexpected status %d", resp.StatusCode)
	}

	var out generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("turn: decode: %w", err)
	}
	if len(out.IceServers) == 0 {
		return nil, errors.New("turn: response contained no ice servers")
	}
	return out.IceServers, nil
}

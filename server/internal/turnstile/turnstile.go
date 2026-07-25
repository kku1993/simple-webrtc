// Package turnstile implements the Cloudflare Turnstile siteverify call
// described in docs/DESIGN.md §"Bot prevention".
//
// Only create-room is gated. The Verify method posts the client-supplied token
// and the resolved client IP to Cloudflare and reports whether the token is
// valid. Tokens are single-use; callers must obtain a fresh one on failure.
package turnstile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Client verifies Cloudflare Turnstile tokens.
type Client struct {
	secretKey  string
	httpClient *http.Client
	endpoint   string
}

// New constructs a Client with the given secret key.
func New(secretKey string) *Client {
	return &Client{
		secretKey:  secretKey,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		endpoint:   "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	}
}

// SetEndpoint overrides the siteverify endpoint, primarily for tests.
func (c *Client) SetEndpoint(u string) { c.endpoint = u }

// Verify posts the token and remoteIP to Cloudflare. Returns true if Cloudflare
// reports the token as valid.
func (c *Client) Verify(ctx context.Context, token, remoteIP string) (bool, error) {
	if c.secretKey == "" {
		return false, errors.New("turnstile: secret key not configured")
	}
	form := url.Values{
		"secret":   {c.secretKey},
		"response": {token},
	}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return false, fmt.Errorf("turnstile: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("turnstile: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("turnstile: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Success     bool     `json:"success"`
		ErrorCodes  []string `json:"error-codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("turnstile: decode: %w", err)
	}
	return body.Success, nil
}

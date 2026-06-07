// Package providers contains the outbound HTTP client the gateway uses to call
// the actual provider backends (in v0.1 these are the local mock providers).
//
// A single *http.Client is created once and REUSED for all calls. The Go
// standard library docs are explicit that http.Client / Transport are safe for
// concurrent use and should be reused — creating a fresh client per request
// leaks connections and kills performance.
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ChatResponse is the (simplified) JSON shape every provider returns.
//
//	{ "provider": "provider-a", "message": "Mock AI response from provider-a" }
type ChatResponse struct {
	Provider string `json:"provider"`
	Message  string `json:"message"`
}

// Client is a thin, reusable wrapper around *http.Client.
type Client struct {
	http *http.Client
}

// NewClient builds a Client with a sensible per-request timeout so a slow or
// dead provider can never hang the gateway forever.
func NewClient(timeout time.Duration) *Client {
	return &Client{
		http: &http.Client{Timeout: timeout},
	}
}

// Complete forwards the chat payload to <baseURL>/v1/chat/completions and parses
// the provider's JSON response.
//
// The context lets the caller propagate deadlines/cancellation end-to-end: if
// the client disconnects, the in-flight provider call is cancelled too.
func (c *Client) Complete(ctx context.Context, baseURL string, payload any) (*ChatResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call provider %s: %w", url, err)
	}
	defer resp.Body.Close()

	// Cap the response we read at 1 MiB to protect against a misbehaving backend.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}

	// Treat any non-2xx as a provider failure (the router/gateway can then 502).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider returned status %d: %s", resp.StatusCode, string(raw))
	}

	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode provider response: %w", err)
	}
	return &out, nil
}

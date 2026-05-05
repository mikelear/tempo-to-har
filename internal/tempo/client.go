// Package tempo is a minimal HTTP client for Grafana Tempo's query API.
//
// Two operations the synthesizer needs:
//   - Search: trace IDs matching service.name within a window
//   - Trace: full span tree for a trace ID (OTLP format)
//
// Tempo API spec: https://grafana.com/docs/tempo/latest/api_docs/
//
// Spike scope: in-cluster Tempo at tempo.jx-observability:3200,
// no auth required. Phase 1 hardening: cross-cluster auth, retry/backoff,
// pagination for large result sets.
package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client wraps a Tempo HTTP endpoint.
type Client struct {
	BaseURL string // e.g. http://tempo.jx-observability.svc.cluster.local:3200
	HTTP    *http.Client
}

// New returns a Client with sensible defaults.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// TraceRef is a search-result entry (subset of Tempo's full result shape).
type TraceRef struct {
	TraceID           string `json:"traceID"`
	RootServiceName   string `json:"rootServiceName"`
	RootTraceName     string `json:"rootTraceName"`
	StartTimeUnixNano string `json:"startTimeUnixNano"`
	DurationMs        int    `json:"durationMs"`
}

// SearchResult is the top-level shape of /api/search.
type SearchResult struct {
	Traces []TraceRef `json:"traces"`
}

// Search returns trace IDs matching service.name within the given window.
// Tempo's /api/search supports `tags` for resource attribute filters and
// `start`/`end` as Unix timestamps in seconds.
func (c *Client) Search(ctx context.Context, serviceName string, since, until time.Time, limit int) ([]TraceRef, error) {
	q := url.Values{}
	q.Set("tags", fmt.Sprintf("service.name=%s", serviceName))
	q.Set("start", strconv.FormatInt(since.Unix(), 10))
	q.Set("end", strconv.FormatInt(until.Unix(), 10))
	q.Set("limit", strconv.Itoa(limit))

	u := fmt.Sprintf("%s/api/search?%s", c.BaseURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempo search %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tempo search returned %d: %s", resp.StatusCode, string(body))
	}

	var result SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode search result: %w", err)
	}
	return result.Traces, nil
}

// Trace returns the full span tree for a trace ID. Returns the raw JSON
// (OTLP-formatted) — caller is responsible for unmarshaling into the
// shape they need (we use a minimal subset in internal/synth).
func (c *Client) Trace(ctx context.Context, traceID string) (json.RawMessage, error) {
	u := fmt.Sprintf("%s/api/traces/%s", c.BaseURL, traceID)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempo trace %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tempo trace returned %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read trace body: %w", err)
	}
	return body, nil
}

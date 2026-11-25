package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	pathWorkerReady  = "/api/internal/worker"
	pathJobRunning   = "/api/internal/routing-jobs/%s/running"
	pathJobSucceeded = "/api/internal/routing-jobs/%s/succeeded"
	pathJobFailed    = "/api/internal/routing-jobs/%s/failed"
	pathCacheLookup  = "/api/internal/isochrone-cache/lookup"
	pathCachePut     = "/api/internal/isochrone-cache"
)

const DefaultTimeout = 30 * time.Second

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

var (
	_ RoutingJobStore = (*Client)(nil)
	_ IsochroneCache  = (*Client)(nil)
)

func Connect(ctx context.Context, apiURL, token string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	base, err := normalizeBaseURL(apiURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		baseURL:    base,
		token:      token,
		httpClient: &http.Client{Timeout: timeout},
	}
	if err := c.ping(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("store: API_URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("store: parsing API_URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("store: parsing API_URL: %q is not an absolute URL", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

func (c *Client) ping(ctx context.Context) error {
	resp, _, err := c.do(ctx, http.MethodGet, pathWorkerReady, nil)
	if err != nil {
		return fmt.Errorf("store: pinging API: %w", err)
	}
	defer drain(resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("store: WORKER_TOKEN was rejected by the API")
	case http.StatusNotFound:
		return fmt.Errorf("store: API does not expose the worker endpoints at %s", c.baseURL)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("store: worker API is not configured on the API")
	default:
		return fmt.Errorf("store: pinging API: status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}
}

func (c *Client) Heartbeat(ctx context.Context, every time.Duration, failed func(error)) error {
	// The beat is how the API tells an idle worker from a dead one (SPA-442).
	// A failed beat is reported and the next one tried: the API being away is
	// what its routing status reports, not a reason for the worker to exit.
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		// A beat that hangs must not hold up the next one.
		beatCtx, cancel := context.WithTimeout(ctx, every)
		err := c.ping(beatCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			failed(err)
		}
	}
}

func (c *Client) MarkRunning(ctx context.Context, jobID string) error {
	return c.transition(ctx, fmt.Sprintf(pathJobRunning, url.PathEscape(jobID)), jobID, nil)
}

func (c *Client) MarkSucceeded(ctx context.Context, jobID string, done Succeeded) error {
	return c.transition(ctx, fmt.Sprintf(pathJobSucceeded, url.PathEscape(jobID)), jobID, jobSucceededBody(done))
}

func (c *Client) MarkFailed(ctx context.Context, jobID, message string) error {
	return c.transition(ctx, fmt.Sprintf(pathJobFailed, url.PathEscape(jobID)), jobID, jobFailedBody{Error: message})
}

type jobSucceededBody struct {
	Result        json.RawMessage `json:"result"`
	TilesetAt     time.Time       `json:"tileset_at,omitzero"`
	ReusableUntil time.Time       `json:"reusable_until,omitzero"`
}

type jobFailedBody struct {
	Error string `json:"error"`
}

func (c *Client) transition(ctx context.Context, path, jobID string, body any) error {
	resp, sent, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return fmt.Errorf("store: updating routing job %s: %w", jobID, err)
	}
	defer drain(resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: updating routing job %s: %d bytes", ErrBodyTooLarge, jobID, sent)
	default:
		return fmt.Errorf("store: updating routing job %s: status %d: %s", jobID, resp.StatusCode, readErrorBody(resp.Body))
	}
}

type cacheLookupRequest struct {
	Keys []IsochroneKey `json:"keys"`
}

type cacheLookupResponse struct {
	Entries []cacheLookupEntry `json:"entries"`
}

type cacheLookupEntry struct {
	Key       IsochroneKey    `json:"key"`
	Geometry  json.RawMessage `json:"geometry"`
	TilesetAt time.Time       `json:"tileset_at,omitzero"`
}

type cachePutRequest struct {
	Entries []CachedIsochrone `json:"entries"`
}

func (c *Client) Get(ctx context.Context, keys []IsochroneKey) (map[IsochroneKey]CachedIsochrone, error) {
	found := make(map[IsochroneKey]CachedIsochrone, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	resp, _, err := c.do(ctx, http.MethodPost, pathCacheLookup, cacheLookupRequest{Keys: keys})
	if err != nil {
		return nil, fmt.Errorf("store: reading isochrone cache: %w", err)
	}
	defer drain(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("store: reading isochrone cache: status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var out cacheLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("store: reading isochrone cache: %w", err)
	}
	for _, e := range out.Entries {
		found[e.Key] = CachedIsochrone(e)
	}
	return found, nil
}

func (c *Client) Put(ctx context.Context, entries []CachedIsochrone) error {
	if len(entries) == 0 {
		return nil
	}

	resp, sent, err := c.do(ctx, http.MethodPost, pathCachePut, cachePutRequest{Entries: entries})
	if err != nil {
		return fmt.Errorf("store: writing isochrone cache: %w", err)
	}
	defer drain(resp.Body)
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return fmt.Errorf("%w: writing isochrone cache: %d bytes", ErrBodyTooLarge, sent)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("store: writing isochrone cache: status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, int, error) {
	// The byte count is what a 413 reports, so the next body the API turns
	// away says how far past its cap it was (SPA-561).
	var rdr io.Reader
	sent := 0
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
		sent = len(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	return resp, sent, err
}

func drain(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 4096))
	_ = r.Close()
}

func readErrorBody(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 4096))
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return body.Error
	}
	return strings.TrimSpace(string(raw))
}

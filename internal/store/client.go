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

// The HTTP contract this client speaks. sparks-effect-api owns the other half.
// The two repositories share no Go code, so the paths and bodies are the
// agreement — the same shape of contract as the queued message, just over
// HTTPS instead of AMQP, and authenticated with a shared bearer token rather
// than by being on a private broker.
//
//	GET  /api/internal/worker
//	     204 with a valid token. The startup ping; a 401 here means the
//	     token is wrong, which will never recover, so the process refuses
//	     to boot rather than consuming jobs it cannot record.
//
//	POST /api/internal/routing-jobs/{id}/running
//	     204, or 404 which this client reports as ErrJobNotFound. The API
//	     treats a terminal job the same as a missing one: a late redelivery
//	     must not drag a finished job back to running.
//
//	POST /api/internal/routing-jobs/{id}/succeeded
//	     body {"result": <json>}. 204 or 404.
//
//	POST /api/internal/routing-jobs/{id}/failed
//	     body {"error": "<message>"}. 204 or 404.
//
//	POST /api/internal/isochrone-cache/lookup
//	     body {"keys": [<IsochroneKey>...]}. 200 {"entries":[{"key":...,"geometry":...}]}
//
//	POST /api/internal/isochrone-cache
//	     body {"entries": [<CachedIsochrone>...]}. 204. Conflicts are left
//	     alone, matching the ON CONFLICT DO NOTHING the SQL used to do.
const (
	pathWorkerReady  = "/api/internal/worker"
	pathJobRunning   = "/api/internal/routing-jobs/%s/running"
	pathJobSucceeded = "/api/internal/routing-jobs/%s/succeeded"
	pathJobFailed    = "/api/internal/routing-jobs/%s/failed"
	pathCacheLookup  = "/api/internal/isochrone-cache/lookup"
	pathCachePut     = "/api/internal/isochrone-cache"
)

// DefaultTimeout bounds one call to the API. Job transitions are small; a
// cache put of many polygons is the largest body this client sends, and thirty
// seconds is long enough for that without holding a job slot indefinitely
// against an API that is not answering.
const DefaultTimeout = 30 * time.Second

// Client is an HTTP-backed RoutingJobStore and IsochroneCache over the API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

var (
	_ RoutingJobStore = (*Client)(nil)
	_ IsochroneCache  = (*Client)(nil)
)

// Connect builds a Client against apiURL and verifies the token is accepted.
//
// The ping is what used to be pool.Ping: a worker that cannot record results
// should not start consuming, because it would run indefinitely doing work it
// has nowhere to put. A missing or rejected token is the same class of
// failure as a DATABASE_URL that would not authenticate — refuse to boot.
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
	resp, err := c.do(ctx, http.MethodGet, pathWorkerReady, nil)
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

// MarkRunning moves a job to running.
//
// It is the one transition that is allowed to be a no-op. A job redelivered
// after a crash is already running, and refusing to re-mark it would fail work
// that is otherwise perfectly recoverable — so the API matches any status
// short of terminal rather than requiring `queued`. Terminal statuses are
// excluded so a late redelivery cannot drag a finished job backwards, and
// that case arrives here as ErrJobNotFound, the same as a missing row.
func (c *Client) MarkRunning(ctx context.Context, jobID string) error {
	return c.transition(ctx, fmt.Sprintf(pathJobRunning, url.PathEscape(jobID)), jobID, nil)
}

// MarkSucceeded records the computed isochrone and completes the job.
//
// This is the write the consumer acks behind: until it lands, a crash
// redelivers the message and the chain runs again. That is safe because the
// write is an unconditional overwrite keyed by id — recomputing an isochrone
// over an immutable graph produces the same answer, so a second write is
// indistinguishable from the first.
func (c *Client) MarkSucceeded(ctx context.Context, jobID string, result json.RawMessage) error {
	return c.transition(ctx, fmt.Sprintf(pathJobSucceeded, url.PathEscape(jobID)), jobID, jobSucceededBody{Result: result})
}

// MarkFailed records why a job cannot be completed.
//
// It is what stops a client polling forever, so it is called on every path that
// abandons a job — a message the worker cannot read, a request Valhalla
// refuses, retries exhausted. The message is stored as given; callers are
// expected to have wrapped it into something a person can act on.
func (c *Client) MarkFailed(ctx context.Context, jobID, message string) error {
	return c.transition(ctx, fmt.Sprintf(pathJobFailed, url.PathEscape(jobID)), jobID, jobFailedBody{Error: message})
}

type jobSucceededBody struct {
	Result json.RawMessage `json:"result"`
}

type jobFailedBody struct {
	Error string `json:"error"`
}

func (c *Client) transition(ctx context.Context, path, jobID string, body any) error {
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return fmt.Errorf("store: updating routing job %s: %w", jobID, err)
	}
	defer drain(resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
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
	Key      IsochroneKey    `json:"key"`
	Geometry json.RawMessage `json:"geometry"`
}

type cachePutRequest struct {
	Entries []CachedIsochrone `json:"entries"`
}

// Get looks every key up in one round trip.
//
// A key with no row is simply absent from the map. The keys it returns are
// the caller's own, echoed back by the API rather than re-read off storage,
// so a caller that spelled a uuid with different casing still finds the hit
// in its own map.
func (c *Client) Get(ctx context.Context, keys []IsochroneKey) (map[IsochroneKey]json.RawMessage, error) {
	found := make(map[IsochroneKey]json.RawMessage, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	resp, err := c.do(ctx, http.MethodPost, pathCacheLookup, cacheLookupRequest{Keys: keys})
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
		found[e.Key] = e.Geometry
	}
	return found, nil
}

// Put writes the batch in one round trip.
//
// An entry whose key is already present is left alone: a conflict means
// another worker chained over the same graph concurrently and computed the
// same inputs into the same polygon. Overwriting it would churn the row
// to no effect.
func (c *Client) Put(ctx context.Context, entries []CachedIsochrone) error {
	if len(entries) == 0 {
		return nil
	}

	resp, err := c.do(ctx, http.MethodPost, pathCachePut, cachePutRequest{Entries: entries})
	if err != nil {
		return fmt.Errorf("store: writing isochrone cache: %w", err)
	}
	defer drain(resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("store: writing isochrone cache: status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
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

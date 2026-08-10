package valhalla

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Valhalla's native endpoint paths. Unlike the Stadia client this replaces,
// both hang off one base URL: there is one server, not two vendor products.
const (
	isochronePath = "/isochrone"
	matrixPath    = "/sources_to_targets"
	routePath     = "/route"
	statusPath    = "/status"
)

// DefaultTimeout bounds a single Valhalla call. A chain issues many of them
// concurrently and the whole chain is bounded by the job's own context, so this
// is about not letting one wedged request hold a slot in the fan-out
// indefinitely.
const DefaultTimeout = 30 * time.Second

// LocationRadiusM and LocationMinReachability widen every location Valhalla is
// asked to correlate to its graph, on every call this client makes (SPA-234).
//
// Valhalla snaps a location to the nearest candidate edge within radius
// metres, then discards any candidate reaching fewer than minimum_reachability
// nodes — a disconnected island, such as a construction-site driveway not yet
// linked to the public network — and searches again among what is left. Left
// at Valhalla's own defaults (radius effectively 0, i.e. only the very nearest
// edges; minimum_reachability 50), a point near nothing but a small isolated
// fragment finds no candidate that survives, and the request is refused with
// "Locations are in unconnected regions" rather than routed against a nearby
// edge that actually leads somewhere.
//
// Widening radius is the primary lever: it is what gives Valhalla a real
// connected edge to fall back to at all, a few hundred metres out, rather than
// only the immediate fragment. minimum_reachability is lowered alongside it,
// modestly rather than to zero, so a genuinely small but real local road
// network (a short dead-end, a modest development) still qualifies — the
// point is to stop rejecting isolated fragments near a point of interest, not
// to accept routing onto something with no way out.
//
// This is general hardening, not specific to any one station: it helps every
// point that happens to land near a small disconnected OSM fragment —
// driveways, parking lots, construction sites — of which the Las Vegas
// terminus (still mid-construction; see Station.RoutingLocation in
// sparks-effect-api) is the first one this repository has actually hit.
const (
	LocationRadiusM         = 500
	LocationMinReachability = 15
)

// HTTPClient talks to a Valhalla server over HTTP.
//
// There is no API key and no Authorization header. The server is a ClusterIP
// service with no ingress, so the network is the authorization: only pods in
// the cluster can reach it at all.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

var _ Client = (*HTTPClient)(nil)

// NewHTTPClient builds a client for a Valhalla base URL, e.g.
// http://valhalla:8002. A trailing slash is tolerated.
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{Timeout: DefaultTimeout},
	}
}

// WithTimeout overrides the per-request timeout and returns the client for
// chaining.
func (c *HTTPClient) WithTimeout(d time.Duration) *HTTPClient {
	c.httpClient.Timeout = d
	return c
}

// location builds one Valhalla location object, widened by
// LocationRadiusM/LocationMinReachability (see their doc comment) so a point
// near a small disconnected OSM fragment still correlates to the graph.
func location(p LatLng) map[string]any {
	return map[string]any{
		"lat":                  p.Lat,
		"lon":                  p.Lng,
		"radius":               LocationRadiusM,
		"minimum_reachability": LocationMinReachability,
	}
}

func (c *HTTPClient) Isochrone(ctx context.Context, req IsochroneRequest) (*IsochroneResponse, error) {
	// Valhalla's contour time is in minutes and is minute-granular. The
	// chainer never asks for less than a minute (see minIsoContourSecs there),
	// so this truncation cannot produce the {"time": 0} that Valhalla rejects.
	body := map[string]any{
		"locations": []map[string]any{location(req.Origin)},
		"costing":   req.Costing,
		"contours":  []map[string]any{{"time": req.BudgetSecs / 60}},
		"polygons":  true,
	}
	var resp IsochroneResponse
	if err := c.post(ctx, isochronePath, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) Matrix(ctx context.Context, req MatrixRequest) (*MatrixResponse, error) {
	sources := make([]map[string]any, len(req.Origins))
	for i, o := range req.Origins {
		sources[i] = location(o)
	}
	targets := make([]map[string]any, len(req.Destinations))
	for i, d := range req.Destinations {
		targets[i] = location(d)
	}
	body := map[string]any{
		"sources": sources,
		"targets": targets,
		"costing": req.Costing,
	}
	var resp MatrixResponse
	if err := c.post(ctx, matrixPath, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// routeBody is the slice of Valhalla's /route response this client reads. The
// rest of it — maneuvers, narrative, per-leg summaries — is a turn-by-turn
// answer to a question nobody here is asking.
type routeBody struct {
	Trip struct {
		Legs []struct {
			Shape string `json:"shape"`
		} `json:"legs"`
	} `json:"trip"`
}

// minDrawableShapePoints is the fewest positions that are a line rather than a
// point. Valhalla answering with less than this is a malfunction, not a walk of
// zero length: a route it could not find is a 400, which never reaches here.
const minDrawableShapePoints = 2

func (c *HTTPClient) Route(ctx context.Context, req RouteRequest) (*RouteResponse, error) {
	body := map[string]any{
		"locations": []map[string]any{location(req.Origin), location(req.Destination)},
		"costing":   req.Costing,
	}
	var resp routeBody
	if err := c.post(ctx, routePath, body, &resp); err != nil {
		return nil, err
	}

	var shape []LatLng
	for _, leg := range resp.Trip.Legs {
		points, err := decodePolyline6(leg.Shape)
		if err != nil {
			return nil, fmt.Errorf("%w: decode route shape: %v", ErrUpstream, err)
		}
		// A leg begins on the point the one before it ended on, so the shared
		// position is dropped rather than emitted twice. Two locations produce
		// one leg, so this only matters if Valhalla ever splits the walk.
		if len(shape) > 0 && len(points) > 0 {
			points = points[1:]
		}
		shape = append(shape, points...)
	}
	if len(shape) < minDrawableShapePoints {
		// Transient rather than permanent: the request was well-formed and was
		// answered 200, so what went wrong is the server, and the same bytes
		// may well be answered properly a moment later.
		return nil, fmt.Errorf("%w: route carried no drawable shape", ErrUpstream)
	}
	return &RouteResponse{Shape: shape}, nil
}

// statusBody is the default (non-verbose) /status response. Valhalla reports
// more when service_limits.status.allow_verbose is on, which the deployed
// container leaves off; the two fields below are present either way.
type statusBody struct {
	Version             string `json:"version"`
	TilesetLastModified int64  `json:"tileset_last_modified"`
}

func (c *HTTPClient) Status(ctx context.Context) (Status, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+statusPath, nil)
	if err != nil {
		return Status{}, fmt.Errorf("%w: build status request: %v", ErrUpstream, err)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Status{}, classify(resp.StatusCode, readErrorBody(resp.Body))
	}
	var body statusBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Status{}, fmt.Errorf("%w: decode status: %v", ErrUpstream, err)
	}

	out := Status{Version: body.Version}
	if body.TilesetLastModified > 0 {
		out.TilesetAt = time.Unix(body.TilesetLastModified, 0).UTC()
	}
	return out, nil
}

// errorBody is Valhalla's error shape. error_code is its own catalogue of
// causes (150 is too many matrix pairs, 154 a pair too far apart); it is
// carried into the message so a failed job says which limit it hit.
type errorBody struct {
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
}

func (c *HTTPClient) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		// Marshalling a map we built ourselves cannot fail on input; treating
		// this as permanent would misclassify a bug as the caller's fault.
		return fmt.Errorf("%w: marshal request: %v", ErrUpstream, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Unreachable, refused, timed out, context cancelled: all transient
		// from this layer's point of view. A cancelled job is abandoned by the
		// consumer before the retry decision is reached.
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return classify(resp.StatusCode, readErrorBody(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrUpstream, err)
	}
	return nil
}

// readErrorBody extracts Valhalla's error message, falling back to nothing
// when the body is not the shape documented. Reads are bounded: an error body
// is a sentence, and a server malfunctioning badly enough to stream megabytes
// should not be able to exhaust this process's memory.
func readErrorBody(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 4096))
	var body errorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return strings.TrimSpace(string(raw))
	}
	if body.ErrorCode != 0 {
		return fmt.Sprintf("%s (error_code %d)", body.Error, body.ErrorCode)
	}
	return body.Error
}

// classify maps an HTTP status onto the three failure modes callers act on.
//
// 400 is the only permanent one, and it is permanent because Valhalla returns
// it for exactly the things that will not change on a retry: a contour past
// the ceiling, too many matrix pairs, locations further apart than the costing
// allows, a costing it does not implement. Everything else — including a 404,
// which here means a path this client got wrong rather than a missing
// resource — is treated as the server not answering, and retried.
func classify(status int, msg string) error {
	if msg == "" {
		msg = fmt.Sprintf("status %d", status)
	}
	switch status {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrRateLimit, msg)
	default:
		return fmt.Errorf("%w: status %d: %s", ErrUpstream, status, msg)
	}
}

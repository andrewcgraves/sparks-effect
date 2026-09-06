package valhalla

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	// Embedded so a departure date is resolved on the corridor's calendar even
	// in an image carrying no zoneinfo database. See ServiceAreaZone.
	_ "time/tzdata"
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

// TransitStartEndMaxDistanceM is the walking distance allowed at the start
// or end of a multimodal route. Valhalla's request default is 2415 m (1.5
// miles, kMultimodalStartEndMaxDistance), which clips suburban walks to a
// station. 10 km is the walk we actually want, and it sits under the 15 km
// service-limit ceiling pedestrian.max_multimodal_walking_distance, so the
// request needs no matching config change (SPA-246, SPA-297).
//
// The wire key is transit_start_end_max_distance. Valhalla's docs renamed it
// to multimodal_start_end_max_distance, but PedestrianCost still copies the
// transit_* protobuf field into the engine, and the live server drops the
// renamed key without a diagnostic — which is how every multimodal request
// we ever made ran at the 2415 m default (SPA-297).
const TransitStartEndMaxDistanceM = 10000

// dateTimeDepartAt is Valhalla's date_time.type for a specified departure.
// Type 0 is "now", which waits for the next trip rather than a representative
// commute; type 1 is "depart at" the value below.
const dateTimeDepartAt = 1

const dateTimeLayout = "2006-01-02T15:04"

// dateLayout is the service date a transit answer belongs to — the GTFS
// calendar's own spelling, and the isochrone cache's departs_on column.
const dateLayout = "2006-01-02"

// serviceAreaTZ is the IANA zone the corridor's origins stand in.
//
// It is a constant rather than a lookup because resolving an arbitrary point to
// a zone needs a timezone shapefile, which is a dependency and a data file this
// worker does not otherwise want. Every scenario it serves is in California or
// Nevada, both of them wholly Pacific, so the constant is exact for every
// origin it will be asked about today. A corridor outside Pacific time is what
// would make the lookup worth its weight; until then this is the honest
// spelling of what the code already assumes.
const serviceAreaTZ = "America/Los_Angeles"

// ServiceAreaZone is the calendar a departure date is resolved on.
//
// tzdata is embedded rather than read from the host, because the worker runs in
// a distroless-ish image that need not carry a zoneinfo database, and a missing
// one would silently degrade every departure to UTC — which is the bug this
// exists to fix. The fixed-offset fallback below is therefore unreachable in
// practice; it is standing Pacific Standard Time, an hour out during DST but
// never a day out, which is the failure worth having.
var ServiceAreaZone = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation(serviceAreaTZ); err == nil {
		return loc
	}
	return time.FixedZone("PST", -8*60*60)
})

// WeekdayDepartAt is the representative departure a multimodal request uses
// when the caller has not picked one: 08:00 on the next weekday (today, if
// today is one), on loc's calendar.
//
// date_time is load-bearing for transit costing. The same pair was 781 s at
// a weekday morning versus 3480 s without a time (waiting for the next trip).
// Valhalla interprets the value in the origin's local timezone from its own
// timezone tiles, so the string carries no offset — but *which day* it names
// still has to be decided here, and deciding it on the UTC calendar meant the
// date rolled over at about 17:00 Pacific. The same pin at the same budget
// then drew a different surface in the evening than it did at lunchtime, a
// Friday evening asked for Monday, and a request made the evening before a
// feed's end_date asked for a day that feed does not cover (SPA-274).
func WeekdayDepartAt(now time.Time, loc *time.Location) time.Time {
	d := now.In(loc)
	t := time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, loc)
	for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// ServiceDate names the GTFS service day a departure falls on.
//
// It is what the isochrone cache keys a transit row by, and it is derived from
// the departure the request actually carried rather than recomputed from the
// clock: a key computed separately from the request it describes is a key that
// can disagree with it, which is how a cache serves one date's polygon for
// another's question (SPA-269).
func ServiceDate(departAt time.Time) string {
	return departAt.Format(dateLayout)
}

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

// attachMultimodalOptions adds the request fields multimodal costing needs
// and pedestrian/bicycle/auto must not send.
//
// date_time is the caller's departure, which is how one chain holds one
// departure throughout and how an egress polygon is drawn at the hour the
// rider actually steps off rather than at the morning peak (SPA-274).
//
// A zero departure is refused rather than defaulted, and that is deliberate:
// the departure is a decision, and there must be exactly one place that makes
// it. The chainer resolves it once per chain and the cache key is derived from
// the same value, so a second default here could hand the wire a different
// moment than the key names — which is the divergence ServiceDate exists to
// prevent. Refusing locally also keeps a mistaken caller off the wire, the way
// Matrix refuses multimodal, and lands in the same permanent bucket: sending
// the zero time would ask Valhalla to depart in the year 1, and omitting
// date_time entirely makes it wait for the next trip, which measured 3480 s
// against 781 s on the same pair.
//
// transit_start_end_max_distance raises the 2415 m walk clip so a suburban
// origin can still reach a station on foot before boarding (SPA-246). The
// name is load-bearing: Valhalla silently drops unrecognised costing-option
// keys, and multimodal_start_end_max_distance is one of those (SPA-297).
func attachMultimodalOptions(body map[string]any, costing Costing, departAt time.Time) error {
	if costing != CostingMultimodal {
		return nil
	}
	if departAt.IsZero() {
		return fmt.Errorf("%w: multimodal costing needs a departure; see WeekdayDepartAt", ErrBadRequest)
	}
	body["date_time"] = map[string]any{
		"type":  dateTimeDepartAt,
		"value": departAt.Format(dateTimeLayout),
	}
	body["costing_options"] = map[string]any{
		"pedestrian": map[string]any{
			"transit_start_end_max_distance": TransitStartEndMaxDistanceM,
		},
	}
	return nil
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
	if err := attachMultimodalOptions(body, req.Costing, req.DepartAt); err != nil {
		return nil, err
	}
	var resp IsochroneResponse
	if err := c.post(ctx, isochronePath, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) Matrix(ctx context.Context, req MatrixRequest) (*MatrixResponse, error) {
	if !req.Costing.SupportsMatrix() {
		// Hard-coded in loki's matrix handler (error 140), not a missing-tiles
		// artefact. Refusing here keeps a mistaken caller off the wire and
		// maps onto the same permanent bucket Valhalla would have returned.
		return nil, fmt.Errorf("%w: sources_to_targets does not support %s costing", ErrBadRequest, req.Costing)
	}
	sources := make([]map[string]any, len(req.Origins))
	for i, o := range req.Origins {
		sources[i] = location(o)
	}
	targets := make([]map[string]any, len(req.Destinations))
	for i, d := range req.Destinations {
		targets[i] = location(d)
	}
	// Units are asked for rather than assumed. Kilometres are Valhalla's own
	// default, but the chainer checks a cell's distance against the straight
	// line to the station it names, and a server configured for miles would
	// make every one of those comparisons wrong by a factor of 1.6.
	body := map[string]any{
		"sources": sources,
		"targets": targets,
		"costing": req.Costing,
		"units":   "kilometers",
	}
	var resp MatrixResponse
	if err := c.post(ctx, matrixPath, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// routeBody is the slice of Valhalla's /route response this client reads. The
// rest of it — the narrative, per-leg summaries, everything on a maneuver but
// its travel mode — is a turn-by-turn answer to a question nobody here is
// asking.
//
// The maneuvers' travel_mode is read for one reason: it is the only place the
// answer says whether the rider boarded anything. See maneuverModeTransit.
type routeBody struct {
	Trip struct {
		Legs []struct {
			Shape     string `json:"shape"`
			Maneuvers []struct {
				TravelMode string `json:"travel_mode"`
			} `json:"maneuvers"`
		} `json:"legs"`
		Summary struct {
			Time   float64 `json:"time"`
			Length float64 `json:"length"`
		} `json:"summary"`
	} `json:"trip"`
}

// maneuverModeTransit is Valhalla's travel_mode for a maneuver spent aboard a
// scheduled service, as opposed to "pedestrian" for the walk at either end.
const maneuverModeTransit = "transit"

// rodeTransit reports whether any maneuver of the trip boarded a service.
//
// A multimodal request Valhalla can find no service for is not an error to it:
// it walks the whole way and answers 200, with a shape and a duration that look
// like any other answer. Every feed in the tiles having an expired calendar
// therefore reads downstream as transit that merely happens to be no better
// than walking (SPA-274).
func (b routeBody) rodeTransit() bool {
	for _, leg := range b.Trip.Legs {
		for _, m := range leg.Maneuvers {
			if m.TravelMode == maneuverModeTransit {
				return true
			}
		}
	}
	return false
}

// minDrawableShapePoints is the fewest positions that are a line rather than a
// point. Valhalla answering with less than this is a malfunction, not a walk of
// zero length: a route it could not find is a 400, which never reaches here.
const minDrawableShapePoints = 2

func (c *HTTPClient) Route(ctx context.Context, req RouteRequest) (*RouteResponse, error) {
	body := map[string]any{
		"locations": []map[string]any{location(req.Origin), location(req.Destination)},
		"costing":   req.Costing,
		"units":     "kilometers",
	}
	if err := attachMultimodalOptions(body, req.Costing, req.DepartAt); err != nil {
		return nil, err
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
	return &RouteResponse{
		Shape:       shape,
		Time:        int(math.Round(resp.Trip.Summary.Time)),
		DistanceKm:  resp.Trip.Summary.Length,
		RodeTransit: resp.rodeTransit(),
	}, nil
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

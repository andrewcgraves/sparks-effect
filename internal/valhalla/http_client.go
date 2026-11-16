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

	_ "time/tzdata"
)

const (
	isochronePath = "/isochrone"
	matrixPath    = "/sources_to_targets"
	routePath     = "/route"
	statusPath    = "/status"
)

const DefaultTimeout = 30 * time.Second

const (
	LocationRadiusM         = 500
	LocationMinReachability = 15
)

const TransitStartEndMaxDistanceM = 10000

const (
	transitNeutralPreference   = 0.5
	transitTransferPenaltySecs = 0
)

const IsochroneDenoise = 0

const dateTimeDepartAt = 1

const dateTimeLayout = "2006-01-02T15:04"

const dateLayout = "2006-01-02"

const serviceAreaTZ = "America/Los_Angeles"

var ServiceAreaZone = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation(serviceAreaTZ); err == nil {
		return loc
	}
	return time.FixedZone("PST", -8*60*60)
})

func WeekdayDepartAt(now time.Time, loc *time.Location) time.Time {
	d := now.In(loc)
	t := time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, loc)
	for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func DepartureRollsOver(departAt time.Time) time.Time {
	// WeekdayDepartAt keeps choosing departAt's date until the midnight that
	// ends it: through the date itself, and through a weekend leading up to a
	// Monday. The API reuses a transit result only until then (SPA-331), so
	// the rule lives beside the clock rather than being re-derived there.
	return time.Date(departAt.Year(), departAt.Month(), departAt.Day()+1, 0, 0, 0, 0, departAt.Location())
}

func ServiceDate(departAt time.Time) string {
	return departAt.Format(dateLayout)
}

type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

var _ Client = (*HTTPClient)(nil)

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{Timeout: DefaultTimeout},
	}
}

func (c *HTTPClient) WithTimeout(d time.Duration) *HTTPClient {
	c.httpClient.Timeout = d
	return c
}

func location(p LatLng) map[string]any {
	return map[string]any{
		"lat":                  p.Lat,
		"lon":                  p.Lng,
		"radius":               LocationRadiusM,
		"minimum_reachability": LocationMinReachability,
	}
}

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
	// Valhalla returns the cheapest path by cost, not the fastest, and an
	// isochrone keeps the seconds of the cheapest-cost label. Its default
	// transit costing (use_bus 0.3, use_rail 0.6, use_transfers 0.3,
	// transfer_penalty 300) prices a bus second at 2.89x a rail second and a
	// transfer at ~14 minutes, so bus-served pins got slow rail routes and
	// shrunken isochrones (SPA-559). At 0.5 every factor normalises to
	// 1.0 and, with no transfer penalty, cost tracks elapsed time: a transfer
	// still costs its 15 s transfer_cost and Valhalla still adds its real
	// 60 s, so the cheapest path becomes the fastest one.
	body["costing_options"] = map[string]any{
		"pedestrian": map[string]any{
			"transit_start_end_max_distance": TransitStartEndMaxDistanceM,
		},
		"transit": map[string]any{
			"use_bus":          transitNeutralPreference,
			"use_rail":         transitNeutralPreference,
			"use_transfers":    transitNeutralPreference,
			"transfer_penalty": transitTransferPenaltySecs,
		},
	}
	return nil
}

func (c *HTTPClient) Isochrone(ctx context.Context, req IsochroneRequest) (*IsochroneResponse, error) {
	body := map[string]any{
		"locations": []map[string]any{location(req.Origin)},
		"costing":   req.Costing,
		"contours":  []map[string]any{{"time": req.BudgetSecs / 60}},
		"polygons":  true,
		"denoise":   IsochroneDenoise,
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

type transitStop struct {
	ArrivalDateTime   string `json:"arrival_date_time"`
	DepartureDateTime string `json:"departure_date_time"`
}

type maneuver struct {
	TravelMode  string  `json:"travel_mode"`
	Time        float64 `json:"time"`
	TransitInfo struct {
		OperatorName string        `json:"operator_name"`
		TransitStops []transitStop `json:"transit_stops"`
	} `json:"transit_info"`
}

type routeBody struct {
	Trip struct {
		Locations []struct {
			DateTime string `json:"date_time"`
		} `json:"locations"`
		Legs []struct {
			Shape     string     `json:"shape"`
			Maneuvers []maneuver `json:"maneuvers"`
		} `json:"legs"`
		Summary struct {
			Time   float64 `json:"time"`
			Length float64 `json:"length"`
		} `json:"summary"`
	} `json:"trip"`
}

const maneuverModeTransit = "transit"

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

func (b routeBody) operators() []string {
	var names []string
	seen := map[string]bool{}
	for _, leg := range b.Trip.Legs {
		for _, m := range leg.Maneuvers {
			name := m.TransitInfo.OperatorName
			if m.TravelMode != maneuverModeTransit || name == "" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

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

		if len(shape) > 0 && len(points) > 0 {
			points = points[1:]
		}
		shape = append(shape, points...)
	}
	if len(shape) < minDrawableShapePoints {
		return nil, fmt.Errorf("%w: route carried no drawable shape", ErrUpstream)
	}
	out := &RouteResponse{
		Shape:       shape,
		Time:        int(math.Round(resp.Trip.Summary.Time)),
		DistanceKm:  resp.Trip.Summary.Length,
		RodeTransit: resp.rodeTransit(),
		Operators:   resp.operators(),
	}
	if req.Costing == CostingMultimodal {
		out.Breakdown, out.BreakdownErr = resp.breakdown(req.DepartAt, out.Time)
	}
	return out, nil
}

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

type errorBody struct {
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
}

func (c *HTTPClient) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: marshal request: %v", ErrUpstream, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
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

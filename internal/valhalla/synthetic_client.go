package valhalla

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
)

// SyntheticClient answers routing calls with geometry instead of tiles.
//
// It is selected in production when no Valhalla URL is configured, which is
// what makes the whole publish → consume → API loop runnable in seconds
// on a laptop. Building a real tileset takes hours and tens of gigabytes, and
// requiring that to check that a message is consumed and a row is written would
// mean the loop was never exercised outside the cluster.
//
// What it produces is deliberately shaped like a real answer and deliberately
// not mistakeable for one. Isochrones are circles — reachability as though the
// road network were an unobstructed plane — and travel times are straight-line
// distance over a fixed speed. Every feature it emits carries
// "synthetic": true in its properties, so a polygon that reaches a map without
// a real server behind it says so.
type SyntheticClient struct {
	// SpeedKmH gives the assumed speed per costing. Zero-valued for a costing
	// this client was not built for, which Isochrone and Matrix reject as bad
	// requests — the same way the real server rejects an unknown costing.
	SpeedKmH map[Costing]float64
}

var _ Client = (*SyntheticClient)(nil)

// NewSyntheticClient builds the stand-in used when no Valhalla server is
// configured.
func NewSyntheticClient() *SyntheticClient {
	return &SyntheticClient{
		SpeedKmH: map[Costing]float64{
			CostingPedestrian: geo.WalkSpeedKmH,
			CostingBicycle:    geo.BikeSpeedKmH,
			CostingAuto:       geo.DriveSpeedKmH,
			CostingMultimodal: geo.TransitSpeedKmH,
		},
	}
}

// syntheticPolygonPoints is how many vertices a synthetic isochrone ring has.
// Enough that it reads as a circle at any zoom a person would look at it, few
// enough that a large fan-out stays small on the wire.
const syntheticPolygonPoints = 36

func (c *SyntheticClient) speed(costing Costing) (float64, error) {
	s, ok := c.SpeedKmH[costing]
	if !ok || s <= 0 {
		return 0, fmt.Errorf("%w: no synthetic speed for costing %q", ErrBadRequest, costing)
	}
	return s, nil
}

func (c *SyntheticClient) Isochrone(_ context.Context, req IsochroneRequest) (*IsochroneResponse, error) {
	speed, err := c.speed(req.Costing)
	if err != nil {
		return nil, err
	}
	if req.BudgetSecs <= 0 {
		return nil, fmt.Errorf("%w: contour must be positive, got %d seconds", ErrBadRequest, req.BudgetSecs)
	}

	radiusKm := speed * (float64(req.BudgetSecs) / 3600.0) / geo.DetourFactor
	ring := circleRing(req.Origin, radiusKm)

	feature := map[string]any{
		"type": "Feature",
		"properties": map[string]any{
			"contour":   req.BudgetSecs / 60,
			"synthetic": true,
		},
		"geometry": map[string]any{
			"type":        "Polygon",
			"coordinates": [][][]float64{ring},
		},
	}
	raw, err := json.Marshal(feature)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal synthetic feature: %v", ErrUpstream, err)
	}

	return &IsochroneResponse{Type: "FeatureCollection", Features: []json.RawMessage{raw}}, nil
}

func (c *SyntheticClient) Matrix(_ context.Context, req MatrixRequest) (*MatrixResponse, error) {
	speed, err := c.speed(req.Costing)
	if err != nil {
		return nil, err
	}

	rows := make([][]MatrixCell, len(req.Origins))
	for i, o := range req.Origins {
		row := make([]MatrixCell, len(req.Destinations))
		for j, d := range req.Destinations {
			roadKm := geo.HaversineKm(o.Lat, o.Lng, d.Lat, d.Lng) * geo.DetourFactor
			row[j] = Cell(int(math.Round(roadKm/speed*3600.0)), roadKm)
		}
		rows[i] = row
	}
	return &MatrixResponse{SourcesToTargets: rows}, nil
}

// Route answers with the straight segment between the two points.
//
// That is the honest answer from a client with no street network: its
// isochrones are circles for the same reason, and inventing bends would put a
// shape on the map that reads as a real walk. The costing is still checked, so
// an unknown one is refused here exactly as the real server refuses it.
func (c *SyntheticClient) Route(_ context.Context, req RouteRequest) (*RouteResponse, error) {
	speed, err := c.speed(req.Costing)
	if err != nil {
		return nil, err
	}
	roadKm := geo.HaversineKm(req.Origin.Lat, req.Origin.Lng, req.Destination.Lat, req.Destination.Lng) * geo.DetourFactor
	return &RouteResponse{
		Shape:      []LatLng{req.Origin, req.Destination},
		Time:       int(math.Round(roadKm / speed * 3600.0)),
		DistanceKm: roadKm,
		// A multimodal answer here is timed at TransitSpeedKmH, so it does claim
		// to have ridden something and says so. Reporting false instead would be
		// the more literal answer — nothing was boarded, because there is no
		// timetable here to board from — but it would make every local run look
		// like the failure this flag exists to detect: a corridor whose feeds
		// have expired answers with exactly that, zero legs that rode (SPA-274).
		RodeTransit: req.Costing == CostingMultimodal,
	}, nil
}

// Status reports ErrNoStatus: there is no tileset behind this client, and
// inventing a timestamp for one would put a fiction in the cache's diagnostic
// column that later reads as fact.
func (c *SyntheticClient) Status(context.Context) (Status, error) {
	return Status{}, ErrNoStatus
}

// circleRing returns a closed GeoJSON linear ring approximating a circle of
// radiusKm about origin, in [lng, lat] order.
//
// Longitude degrees shrink with latitude, so the east-west step is divided by
// cos(lat); without it a circle drawn near the poles would be an ellipse
// stretched east-west. At latitudes past the poles themselves cos approaches
// zero, so the divisor is floored to keep the arithmetic finite.
func circleRing(origin LatLng, radiusKm float64) [][]float64 {
	const kmPerDegreeLat = 110.574

	latDelta := radiusKm / kmPerDegreeLat
	cosLat := math.Cos(origin.Lat * math.Pi / 180)
	if cosLat < 1e-6 {
		cosLat = 1e-6
	}
	lngDelta := latDelta / cosLat

	ring := make([][]float64, 0, syntheticPolygonPoints+1)
	for i := 0; i < syntheticPolygonPoints; i++ {
		theta := 2 * math.Pi * float64(i) / syntheticPolygonPoints
		ring = append(ring, []float64{
			origin.Lng + lngDelta*math.Cos(theta),
			origin.Lat + latDelta*math.Sin(theta),
		})
	}
	// GeoJSON requires the ring to close on its first point exactly.
	ring = append(ring, []float64{ring[0][0], ring[0][1]})
	return ring
}

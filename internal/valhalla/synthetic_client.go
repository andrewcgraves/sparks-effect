package valhalla

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
)

type SyntheticClient struct {
	SpeedKmH map[Costing]float64
}

var _ Client = (*SyntheticClient)(nil)

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

		RodeTransit: req.Costing == CostingMultimodal,
	}, nil
}

func (c *SyntheticClient) Status(context.Context) (Status, error) {
	return Status{}, ErrNoStatus
}

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

	ring = append(ring, []float64{ring[0][0], ring[0][1]})
	return ring
}

package valhalla

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrBadRequest = errors.New("valhalla: bad request")
	ErrRateLimit  = errors.New("valhalla: rate limit exceeded")
	ErrUpstream   = errors.New("valhalla: upstream error")
)

type Costing string

const (
	CostingPedestrian Costing = "pedestrian"
	CostingBicycle    Costing = "bicycle"
	CostingAuto       Costing = "auto"
	CostingMultimodal Costing = "multimodal"
)

func (c Costing) SupportsMatrix() bool {
	switch c {
	case CostingPedestrian, CostingBicycle, CostingAuto:
		return true
	default:
		return false
	}
}

type LatLng struct {
	Lat float64
	Lng float64
}

type IsochroneRequest struct {
	Origin     LatLng
	Costing    Costing
	BudgetSecs int
	DepartAt   time.Time
}

type IsochroneResponse struct {
	Type     string            `json:"type"`
	Features []json.RawMessage `json:"features"`
}

type MatrixRequest struct {
	Origins      []LatLng
	Destinations []LatLng
	Costing      Costing
}

type MatrixCell struct {
	Time       *int     `json:"time"`
	DistanceKm *float64 `json:"distance"`
}

func (c MatrixCell) Routed() bool {
	return c.Time != nil && *c.Time >= 0 && c.DistanceKm != nil && *c.DistanceKm >= 0
}

func Cell(secs int, distanceKm float64) MatrixCell {
	return MatrixCell{Time: &secs, DistanceKm: &distanceKm}
}

type MatrixResponse struct {
	SourcesToTargets [][]MatrixCell `json:"sources_to_targets"`
}

type RouteRequest struct {
	Origin      LatLng
	Destination LatLng
	Costing     Costing
	DepartAt    time.Time
}

type RouteResponse struct {
	Shape       []LatLng
	Time        int
	DistanceKm  float64
	RodeTransit bool
}

type Status struct {
	Version   string
	TilesetAt time.Time
}

type Client interface {
	Isochrone(ctx context.Context, req IsochroneRequest) (*IsochroneResponse, error)
	Matrix(ctx context.Context, req MatrixRequest) (*MatrixResponse, error)
	Route(ctx context.Context, req RouteRequest) (*RouteResponse, error)
	Status(ctx context.Context) (Status, error)
}

var ErrNoStatus = errors.New("valhalla: client reports no tileset status")

type Limits struct {
	MaxMatrixPairs     int
	MaxIsoContourMins  int
	MatrixBLineAutoKm  float64
	MatrixBLineOtherKm float64
	MaxConcurrency     int
}

const (
	DefaultMaxMatrixPairs     = 2500
	DefaultMaxIsoContourMins  = 320
	DefaultMatrixBLineAutoKm  = 400.0
	DefaultMatrixBLineOtherKm = 200.0
	DefaultMaxConcurrency     = 8
)

func DefaultLimits() Limits {
	return Limits{
		MaxMatrixPairs:     DefaultMaxMatrixPairs,
		MaxIsoContourMins:  DefaultMaxIsoContourMins,
		MatrixBLineAutoKm:  DefaultMatrixBLineAutoKm,
		MatrixBLineOtherKm: DefaultMatrixBLineOtherKm,
		MaxConcurrency:     DefaultMaxConcurrency,
	}
}

func (l Limits) BLineLimitKm(c Costing) float64 {
	if c == CostingAuto {
		return l.MatrixBLineAutoKm
	}
	return l.MatrixBLineOtherKm
}

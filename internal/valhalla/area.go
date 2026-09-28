package valhalla

import (
	"encoding/json"
	"fmt"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
)

func (iso *IsochroneResponse) AreaKm2() (float64, error) {
	if iso == nil {
		return 0, fmt.Errorf("nil isochrone")
	}
	var total float64
	parsed := 0
	for _, raw := range iso.Features {
		area, err := featureAreaKm2(raw)
		if err != nil {
			return 0, err
		}
		total += area
		parsed++
	}
	if parsed == 0 {
		return 0, fmt.Errorf("no features")
	}
	return total, nil
}

func featureAreaKm2(raw json.RawMessage) (float64, error) {
	var f struct {
		Geometry struct {
			Type        string          `json:"type"`
			Coordinates json.RawMessage `json:"coordinates"`
		} `json:"geometry"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, err
	}
	switch f.Geometry.Type {
	case "Polygon":
		var coords [][][]float64
		if err := json.Unmarshal(f.Geometry.Coordinates, &coords); err != nil {
			return 0, err
		}
		if err := polygonCoordsUsable(coords); err != nil {
			return 0, err
		}
		return geo.PolygonAreaKm2(coords), nil
	case "MultiPolygon":
		var coords [][][][]float64
		if err := json.Unmarshal(f.Geometry.Coordinates, &coords); err != nil {
			return 0, err
		}
		if len(coords) == 0 {
			return 0, fmt.Errorf("empty coordinates")
		}
		var total float64
		for _, poly := range coords {
			if err := polygonCoordsUsable(poly); err != nil {
				return 0, err
			}
			total += geo.PolygonAreaKm2(poly)
		}
		return total, nil
	default:
		if f.Geometry.Type == "" {
			return 0, fmt.Errorf("missing geometry type")
		}
		return 0, fmt.Errorf("unsupported geometry type %q", f.Geometry.Type)
	}
}

func polygonCoordsUsable(rings [][][]float64) error {
	if len(rings) == 0 || len(rings[0]) == 0 {
		return fmt.Errorf("empty coordinates")
	}
	for _, ring := range rings {
		for _, pos := range ring {
			if len(pos) < 2 {
				return fmt.Errorf("short coordinate position")
			}
		}
	}
	return nil
}

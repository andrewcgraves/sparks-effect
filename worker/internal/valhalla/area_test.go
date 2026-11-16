package valhalla_test

import (
	"encoding/json"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestIsochroneAreaOfShortPositionsIsAnError(t *testing.T) {
	feature, err := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{
			"type":        "Polygon",
			"coordinates": [][][]float64{{{}, {}, {}, {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := (&valhalla.IsochroneResponse{Features: []json.RawMessage{feature}}).AreaKm2()
	if err == nil {
		t.Fatalf("AreaKm2() = %v, want error for a ring of short positions", got)
	}
	if got != 0 {
		t.Errorf("area = %v, want 0 with the error", got)
	}
}

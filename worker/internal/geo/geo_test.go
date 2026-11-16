package geo_test

import (
	"math"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
)

func TestSpeedConstantsMatchTheAPIOriginRangeCheck(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"WalkSpeedKmH", geo.WalkSpeedKmH, 5},
		{"BikeSpeedKmH", geo.BikeSpeedKmH, 15},
		{"DriveSpeedKmH", geo.DriveSpeedKmH, 80},
		{"TransitSpeedKmH", geo.TransitSpeedKmH, 40},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v (must not be smaller than sparks-effect-api's origin-range check)", tc.name, tc.got, tc.want)
		}
	}
}

func TestWalkCircleBoundAtOneHourIsPiRSquared(t *testing.T) {
	got := geo.WalkCircleKm2(60)
	want := 5.1 * 5.1 * math.Pi
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("WalkCircleKm2(60) = %v, want 5.1²π = %v", got, want)
	}
}

func TestPolygonAreaOfAKnownSquare(t *testing.T) {
	got := geo.PolygonAreaKm2([][][]float64{{
		{0, 0}, {0.01, 0}, {0.01, 0.01}, {0, 0.01}, {0, 0},
	}})
	if got < 1.2 || got > 1.3 {
		t.Errorf("area = %.4f km², want ~1.236", got)
	}
}

func TestPolygonAreaOfShortPositionsDoesNotPanic(t *testing.T) {
	got := geo.PolygonAreaKm2([][][]float64{{{}, {}, {}, {}}})
	if got != 0 {
		t.Errorf("area = %v, want 0", got)
	}
}

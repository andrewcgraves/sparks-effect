package geo_test

import (
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

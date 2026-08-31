// Package geo holds the spherical arithmetic shared by the chainer and the
// synthetic routing client.
//
// It is one function and two constants, in its own package only because both
// callers need it and neither is a sensible owner: the chainer would be
// importing the routing client for a distance, or the routing client would be
// importing the chainer.
package geo

import "math"

// earthRadiusKm is the mean radius. Isochrones are drawn from travel times, not
// from distances, so the error a spherical model carries against an ellipsoidal
// one is far below anything visible on the map.
const earthRadiusKm = 6371.0

// DetourFactor converts straight-line distance into road distance. Real road
// networks are not straight, and treating them as though they were would make
// the destination pre-filter drop stations that are in fact reachable.
const DetourFactor = 1.4

// Assumed travel speeds, in km/h.
//
// They are approximations used where a real routed time is not available: the
// chainer sizes its destination pre-filter with them, and the synthetic client
// computes its whole answer from them. Those two have to agree — a station the
// pre-filter admits should be one the synthetic client reports as reachable, or
// a fake-backed run stops exercising the paths a real one does — so the numbers
// live here rather than once in each.
//
// TransitSpeedKmH is the odd one out: it is a blended local-transit speed for
// the access-leg pre-filter, not a physical constant. The API's origin-range
// check uses the same number, and this side must never be the smaller of the
// two copies — a tighter radius here would drop a station the API already
// admitted. 40 km/h is SPA-246's figure.
const (
	WalkSpeedKmH    = 5.0
	BikeSpeedKmH    = 15.0
	DriveSpeedKmH   = 80.0
	TransitSpeedKmH = 40.0
)

// HaversineKm is the great-circle distance between two WGS84 points.
func HaversineKm(lat1, lng1, lat2, lng2 float64) float64 {
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusKm * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

package geo

import "math"

const earthRadiusKm = 6371.0

const DetourFactor = 1.4

const (
	WalkSpeedKmH    = 5.0
	BikeSpeedKmH    = 15.0
	DriveSpeedKmH   = 80.0
	TransitSpeedKmH = 40.0

	valhallaPedestrianKmH = 5.1
)

func HaversineKm(lat1, lng1, lat2, lng2 float64) float64 {
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusKm * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func WalkCircleKm2(contourMins int) float64 {
	// Valhalla's pedestrian default, not the domain 5.0, because this circle
	// is an upper bound on a walked answer.
	r := valhallaPedestrianKmH * float64(contourMins) / 60.0
	return math.Pi * r * r
}

func PolygonAreaKm2(rings [][][]float64) float64 {
	if len(rings) == 0 {
		return 0
	}
	area := math.Abs(ringAreaKm2(rings[0]))
	for _, hole := range rings[1:] {
		area -= math.Abs(ringAreaKm2(hole))
	}
	if area < 0 {
		return 0
	}
	return area
}

func ringAreaKm2(ring [][]float64) float64 {
	if len(ring) < 4 {
		return 0
	}
	var area float64
	n := len(ring) - 1
	for i := 0; i < n; i++ {
		if len(ring[i]) < 2 || len(ring[i+1]) < 2 {
			return 0
		}
		lon1 := ring[i][0] * math.Pi / 180
		lon2 := ring[i+1][0] * math.Pi / 180
		lat1 := ring[i][1] * math.Pi / 180
		lat2 := ring[i+1][1] * math.Pi / 180
		area += (lon2 - lon1) * (2 + math.Sin(lat1) + math.Sin(lat2))
	}
	return area * earthRadiusKm * earthRadiusKm / 2
}

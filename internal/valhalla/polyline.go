package valhalla

import (
	"errors"
	"fmt"
)

const polylinePrecision = 1e6

const maxPolylineChunks = 6

var errMalformedPolyline = errors.New("malformed polyline")

func decodePolyline6(encoded string) ([]LatLng, error) {
	var (
		out      []LatLng
		index    int
		lat, lng int64
	)
	for index < len(encoded) {
		dLat, next, err := decodeSignedValue(encoded, index)
		if err != nil {
			return nil, err
		}
		dLng, next, err := decodeSignedValue(encoded, next)
		if err != nil {
			return nil, err
		}
		index = next

		lat += dLat
		lng += dLng
		out = append(out, LatLng{
			Lat: float64(lat) / polylinePrecision,
			Lng: float64(lng) / polylinePrecision,
		})
	}
	return out, nil
}

func decodeSignedValue(encoded string, index int) (int64, int, error) {
	var (
		result uint64
		shift  uint
	)
	for chunk := 0; ; chunk++ {
		if index >= len(encoded) {
			return 0, index, fmt.Errorf("%w: ran out of input mid-value", errMalformedPolyline)
		}
		if chunk >= maxPolylineChunks {
			return 0, index, fmt.Errorf("%w: value longer than %d chunks", errMalformedPolyline, maxPolylineChunks)
		}
		b := int(encoded[index]) - 63
		index++

		if b < 0 || b > 0x3f {
			return 0, index, fmt.Errorf("%w: byte outside the encodable range", errMalformedPolyline)
		}
		result |= uint64(b&0x1f) << shift
		shift += 5

		if b < 0x20 {
			break
		}
	}

	if result&1 != 0 {
		return -int64(result>>1) - 1, index, nil
	}
	return int64(result >> 1), index, nil
}

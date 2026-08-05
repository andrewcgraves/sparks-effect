package valhalla

import (
	"errors"
	"fmt"
)

// Valhalla sends route shapes as an encoded polyline — the Google algorithm,
// but at six decimal digits rather than the five most implementations default
// to. The difference is a factor of ten in every coordinate, which does not
// fail loudly: a walk decoded at five digits lands in the Atlantic rather than
// erroring, and looks like a routing bug rather than a decoding one.
const polylinePrecision = 1e6

// maxPolylineChunks bounds one value's continuation bytes. Six digits of a
// coordinate is at most 180,000,000, which zig-zags to 29 bits and so needs six
// five-bit chunks; a seventh means the string is not what it claims to be, and
// the bound is what stops a malformed one shifting past the width of the
// accumulator forever.
const maxPolylineChunks = 6

var errMalformedPolyline = errors.New("malformed polyline")

// decodePolyline6 turns an encoded shape into the positions it describes.
//
// Deltas are cumulative and integral: each pair is added to the running
// position in fixed point, and only the total is divided down. Dividing each
// delta as it arrived would accumulate a rounding error along the length of the
// line.
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

// decodeSignedValue reads one zig-zag encoded delta, returning it and the index
// just past it.
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
		// Encoded bytes are 63..126, so b is 0..63. Anything outside that is
		// not a polyline at all — a UTF-8 continuation byte, say — and is
		// rejected here rather than being masked down to five bits and
		// absorbed as data until the chunk bound happens to trip.
		if b < 0 || b > 0x3f {
			return 0, index, fmt.Errorf("%w: byte outside the encodable range", errMalformedPolyline)
		}
		result |= uint64(b&0x1f) << shift
		shift += 5
		// The high bit is the continuation flag; without it this was the last
		// chunk of the value.
		if b < 0x20 {
			break
		}
	}
	// Zig-zag: the low bit is the sign, so odd values are negative and the
	// magnitude is one less than the shifted remainder.
	if result&1 != 0 {
		return -int64(result>>1) - 1, index, nil
	}
	return int64(result >> 1), index, nil
}

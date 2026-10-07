package valhalla

import (
	"fmt"
	"math"
	"time"
)

// The two tolerances guard different things and are deliberately far apart.
//
// ReconcileToleranceSecs bounds how far the stop clocks may disagree with the
// maneuver's seconds about the wait. Valhalla writes every stop clock at minute
// precision, so a wait read off those clocks is honest to within a minute and
// no closer: SPA-335 measured 24 s on a real response, and the ticket asks for
// a sub-minute disagreement to reconcile. Sixty seconds is one clock tick, the
// widest a quantization error can be; anything past it is not rounding, and
// the split is refused.
//
// SummaryDriftToleranceSecs bounds how far the maneuvers' own seconds may sum
// from summary.time. SPA-335 found them to sum exactly, so anything past float
// rounding means the maneuvers do not describe the trip the total does, and
// the walk would otherwise silently absorb the difference.
const (
	ReconcileToleranceSecs    = 60
	SummaryDriftToleranceSecs = 2
)

const dateTimeOffsetLayout = dateTimeLayout + "Z07:00"

func parseClock(raw string) (time.Time, error) {
	// Valhalla writes two forms. The origin's echoed date_time is a bare local
	// wall clock, "2026-09-02T08:00", with its offset in a separate
	// time_zone_offset field; a transit stop's departure_date_time and
	// arrival_date_time carry the offset inline, "2026-09-02T08:31-07:00"
	// (Valhalla's test/gurka/test_gtfs.cc asserts that shape). The request's
	// departAt is sent as the bare form too. Every clock on one access leg is
	// in the same local zone, so all of them are compared as local wall clocks:
	// an offset-bearing clock keeps the local time it names and drops the
	// offset, and the result is pinned to UTC only so the arithmetic is zone
	// free. Converting the stop clocks to an instant instead would put them
	// hours away from a bare origin clock. A trip that crosses a DST change
	// would mismeasure by an hour, which the reconciliation then refuses rather
	// than mis-splits; the chain departs weekdays at 08:00.
	for _, layout := range []string{dateTimeOffsetLayout, dateTimeLayout} {
		t, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("not a %q or %q clock", dateTimeLayout, dateTimeOffsetLayout)
}

func secsDuration(secs float64) time.Duration {
	return time.Duration(secs * float64(time.Second))
}

func (b routeBody) departureClock(departAt time.Time) (time.Time, error) {
	// The anchor is the origin's date_time as Valhalla echoes it — the clock it
	// says it routed from. Failing that, it is the departure exactly as
	// attachMultimodalOptions sent it: formatted to the minute in departAt's own
	// zone and reparsed, because Valhalla departs at :00 of the minute it was
	// sent, not at departAt's seconds. With depart-at (type 1) and an explicit
	// value the two are the same string. departAt is never zero here:
	// attachMultimodalOptions refuses a multimodal request without one before
	// it is sent.
	if len(b.Trip.Locations) > 0 && b.Trip.Locations[0].DateTime != "" {
		at, err := parseClock(b.Trip.Locations[0].DateTime)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: origin date_time %q: %v", ErrUnreconciled, b.Trip.Locations[0].DateTime, err)
		}
		return at, nil
	}
	return parseClock(departAt.Format(dateTimeLayout))
}

func (b routeBody) breakdown(departAt time.Time, totalSecs int) (*Breakdown, error) {
	// A leg that boarded nothing is all walk by definition, whatever its
	// maneuvers sum to and whether or not Valhalla echoed a clock: there is no
	// wait or ride to take out of it, so walk = total holds without measuring.
	if !b.rodeTransit() {
		return &Breakdown{WalkSecs: totalSecs}, nil
	}

	origin, err := b.departureClock(departAt)
	if err != nil {
		return nil, err
	}
	var elapsed, nonTransit, wait, ride float64
	for _, leg := range b.Trip.Legs {
		for _, m := range leg.Maneuvers {
			// JSON carries no NaN or infinity, so a negative time is the only
			// nonsense a decoded maneuver can hold.
			if m.Time < 0 {
				return nil, fmt.Errorf("%w: maneuver time %v", ErrUnreconciled, m.Time)
			}
			if m.TravelMode != maneuverModeTransit {
				nonTransit += m.Time
				elapsed += m.Time
				continue
			}
			w, r, err := m.waitAndRide(origin.Add(secsDuration(elapsed)))
			if err != nil {
				return nil, err
			}
			wait += w
			ride += r
			elapsed += m.Time
		}
	}

	if drift := elapsed - b.Trip.Summary.Time; math.Abs(drift) > SummaryDriftToleranceSecs {
		return nil, fmt.Errorf("%w: maneuvers sum to %.1f s but summary.time is %.1f s",
			ErrUnreconciled, elapsed, b.Trip.Summary.Time)
	}

	// The components must sum to the rounded total exactly, so one of them
	// takes the sub-second residue: the rounding of summary.time, of the wait
	// and of the ride, plus at most SummaryDriftToleranceSecs of float drift.
	// It goes on the walk, so the ride stays the timetabled one and the wait
	// stays what the maneuver holds beyond it; the walk then differs from the
	// sum of the non-transit maneuvers by that residue only.
	waitSecs := int(math.Round(wait))
	rideSecs := int(math.Round(ride))
	walkSecs := totalSecs - waitSecs - rideSecs
	if walkSecs < 0 {
		return nil, fmt.Errorf("%w: wait %d s and ride %d s exceed the %d s total, which walked %.1f s",
			ErrUnreconciled, waitSecs, rideSecs, totalSecs, nonTransit)
	}
	return &Breakdown{WalkSecs: walkSecs, WaitSecs: waitSecs, RideSecs: rideSecs}, nil
}

func (m maneuver) waitAndRide(platform time.Time) (wait, ride float64, err error) {
	// Valhalla folds the platform wait inside the transit maneuver's own time
	// (SPA-335: a 16.7 min maneuver that was a 7.7 min wait and a 9.0 min
	// ride), so the wait is taken out of the maneuver, never added beside it.
	// The ride is the timetabled one, alighting clock minus boarding clock:
	// both are stop clocks off the same timetable at the same precision, and
	// Valhalla's TransitCost charges a transit edge as wait + elapsed_time, so
	// what the maneuver holds beyond the timetabled ride is the wait. Each
	// transit maneuver is one vehicle, so a transfer measures its own wait from
	// the clock at its own start.
	stops := m.TransitInfo.TransitStops
	if len(stops) < 2 {
		return 0, 0, fmt.Errorf("%w: transit maneuver names %d stops; its ride needs where it boarded and where it alighted",
			ErrUnreconciled, len(stops))
	}
	board, err := parseClock(stops[0].DepartureDateTime)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: boarding departure_date_time %q: %v", ErrUnreconciled, stops[0].DepartureDateTime, err)
	}
	last := stops[len(stops)-1]
	alight, err := parseClock(last.ArrivalDateTime)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: alighting arrival_date_time %q: %v", ErrUnreconciled, last.ArrivalDateTime, err)
	}
	if alight.Before(board) {
		return 0, 0, fmt.Errorf("%w: alights %s before it boards %s", ErrUnreconciled,
			alight.Format(dateTimeLayout), board.Format(dateTimeLayout))
	}

	ride = math.Min(alight.Sub(board).Seconds(), m.Time)
	wait = m.Time - ride

	// The reconciliation rule. The wait taken out of the maneuver should be
	// the clock gap from reaching the platform — the departure plus every
	// maneuver before this one — to the boarding stop's departure_date_time.
	// It cannot be exactly: the gap starts at a clock with seconds and ends at
	// a minute-precision one. SPA-335 measured the gap at 8.1 min against a
	// 7.7 min wait, 24 s apart. Within ReconcileToleranceSecs that is the
	// boarding clock's rounding and the maneuver's seconds stand; past it the
	// clocks and the seconds describe different trips and nothing is trusted.
	// The same bound refuses a departure well before the rider reaches the
	// platform, since the wait is never negative.
	gap := board.Sub(platform).Seconds()
	if disagreement := gap - wait; math.Abs(disagreement) > ReconcileToleranceSecs {
		return 0, 0, fmt.Errorf("%w: departs %s, %.0f s after reaching the platform at %s, but the maneuver's %.0f s leaves a %.0f s wait, %.0f s apart",
			ErrUnreconciled, board.Format(dateTimeLayout), gap, platform.Format(time.TimeOnly), m.Time, wait, disagreement)
	}
	return wait, ride, nil
}

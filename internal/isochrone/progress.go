package isochrone

import (
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

// withinBudget reports whether a settled journey arrives inside the budget —
// the one definition of "the rider gets there", read by both the station the
// isochrone reports and the frontier this file walks.
//
// It is a function rather than an expression at two call sites because the two
// readers must agree exactly. Spelled separately they once did not: the egress
// side required a whole minute to spare while the arrival itself needed none,
// and a station between the two got neither a dot nor a stub — which is
// precisely the fault SPA-264 exists to fix, and which would return silently if
// the threshold ever moved in only one of them.
func withinBudget(r transit.Reached, budgetSecs int) bool {
	return r.TotalSecs <= budgetSecs
}

// tripProgress works out how far along each unfinished leg the rider's budget
// would have carried them, and reports how many legs it had to leave out for
// want of a corridor to measure against.
//
// # Which legs get an entry
//
// The tree frontier: one entry per unreached station B whose tree-predecessor A
// the rider does reach — total[B] > budget >= total[A]. That is at most one
// entry per unreached station, exactly one per live branch, and nothing beyond
// the frontier, so a trip that forks sees each fork's ending and the map never
// speculates about stations two or three hops past the budget.
//
// No graph reconstruction is needed for any of it. The search settles every node
// without a budget cap, so both totals are already in hand, and their difference
// folds in the dwell and the once-per-journey boarding wait for free.
//
// # The fraction
//
//	ride_secs = hop_secs - dwell_s
//	avail     = remaining_secs_at_A - board_wait_if_boarding_here
//	fraction  = min(1, avail / ride_secs)          suppressed when avail <= 0
//
// Both deductions follow one principle: remaining time buys motion. A hop's
// seconds are the vehicle moving plus the dwell served on arrival, and that
// dwell is time standing still at a platform the rider never reaches — charging
// it against distance covered would understate every fraction. The boarding wait
// is non-zero only when this hop would be the rider's first ride, which is
// exactly when A was reached on foot; a transfer costs nothing, as the wait
// model already says.
//
// Clamping at one is meaningful rather than defensive. In the band where the
// remaining time covers the ride but not the dwell, the rider physically arrives
// and only fails to complete the modelled stop, so drawing the stub all the way
// to the station is the truthful rendering. It is still not an arrival: nothing
// here touches the reachable set, so the station stays unlit and gets no egress
// polygon.
//
// There is no fraction floor. Any floor would recreate at a smaller scale the
// very fault this fixes, and a floor expressed as a fraction means wildly
// different things on a 400 km hop and an 800 m one.
//
// # What is left out
//
// A hop whose edge names no corridor cannot be drawn, and is skipped. That is
// every hop of a graph compiled before the API emitted the fields, and any hop
// whose stations could not be placed on an alignment. The count comes back so
// the caller can say so once per chain rather than once per leg.
//
// nodes is walked rather than reached, so entries come out in graph order and a
// run over the same graph produces the same list every time.
func tripProgress(nodes []transit.Node, reached map[string]transit.Reached, budgetSecs int) (entries []TripProgress, omitted int) {
	for _, n := range nodes {
		to, settled := reached[n.Slug]
		if !settled || withinBudget(to, budgetSecs) {
			continue // reached, so there is nothing unfinished to report
		}
		if to.PredecessorSlug == "" {
			continue // an access station the rider could not even walk to in time
		}
		from, settled := reached[to.PredecessorSlug]
		if !settled || !withinBudget(from, budgetSecs) {
			continue // past the frontier: the rider does not reach the station this hop starts at
		}

		// The seconds of this hop the budget buys, derived from B's own settled
		// total rather than from A's.
		//
		// That distinction is load-bearing. graphSearch runs one search per
		// access station and keeps, per node, the cheapest journey across all of
		// them — so the best way to reach A and the journey that reaches B
		// *through* A need not be the same trip. Taking the remainder from A's
		// total while taking the boarding wait from B's entry mixed two
		// journeys, and drew the stub further along the alignment than the
		// budget reaches (TestChainer_progressMeasuresTheJourneyThatActually-
		// ReachesTheStation).
		//
		// B's total is the arrival at A on this very journey, plus the hop,
		// plus whatever wait this journey paid to board here. Subtracting it
		// from the budget and adding the hop back therefore leaves exactly the
		// budget that survives to buy motion — with dwell and the
		// once-per-journey boarding wait folded in for free, and no case
		// analysis at all: a wait paid at an earlier station is already spent
		// inside the arrival, and a wait paid to board here is already
		// deducted. A transfer costs nothing; a first ride costs the wait.
		avail := budgetSecs - to.TotalSecs + to.HopSecs
		if avail <= 0 {
			continue // the budget is gone before the doors close; waiting is not travel
		}
		// Counted only once a leg is known to be drawable otherwise, so the
		// figure reported to an operator is legs actually lost to a placeless
		// hop rather than legs that would have been suppressed regardless.
		if to.HopRouteID == "" {
			omitted++
			continue
		}

		// A hop whose dwell exceeds its own seconds is malformed rather than
		// merely degenerate — this is decoded from a queue — and must not put a
		// negative ride on the wire for a consumer that reports how the fraction
		// was arrived at. A zero ride clamps to a whole stub, which is what
		// min(1, avail/0) means.
		rideSecs := to.HopSecs - to.HopDwellS
		if rideSecs < 0 {
			rideSecs = 0
		}
		fraction := 1.0
		if rideSecs > 0 && avail < rideSecs {
			fraction = float64(avail) / float64(rideSecs)
		}

		entries = append(entries, TripProgress{
			FromSlug:      to.PredecessorSlug,
			ToSlug:        n.Slug,
			ServiceID:     to.HopService,
			RouteID:       to.HopRouteID,
			FromChainageM: to.HopFromChainageM,
			ToChainageM:   to.HopToChainageM,
			Fraction:      fraction,
			RemainingSecs: avail,
			RideSecs:      rideSecs,
		})
	}
	return entries, omitted
}

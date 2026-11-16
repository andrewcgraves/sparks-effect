package isochrone

import (
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

func withinBudget(r transit.Reached, budgetSecs int) bool {
	return r.TotalSecs <= budgetSecs
}

func tripProgress(nodes []transit.Node, reached map[string]transit.Reached, budgetSecs int) (entries []TripProgress, omitted int) {
	for _, n := range nodes {
		to, settled := reached[n.Slug]
		if !settled || withinBudget(to, budgetSecs) {
			continue
		}
		if to.PredecessorSlug == "" {
			continue
		}
		from, settled := reached[to.PredecessorSlug]
		if !settled || !withinBudget(from, budgetSecs) {
			continue
		}

		avail := budgetSecs - to.TotalSecs + to.HopSecs
		if avail <= 0 {
			continue
		}

		if to.HopRouteID == "" {
			omitted++
			continue
		}

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

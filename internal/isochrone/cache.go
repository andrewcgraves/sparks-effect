package isochrone

import (
	"context"
	"encoding/json"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// EgressCache is the egress-polygon reuse seam, together with the tileset stamp
// recorded beside anything written through it.
//
// Only the egress leg is cached, and the split is not arbitrary: the origin
// isochrone and the access matrix start from wherever the user clicked, while
// the egress polygons start from station positions the publication fixed. The
// fan-out is also the bulk of the work, one call per station reached by riding
// transit against two for everything else — a walk-only station gets no call
// and nothing to cache. README's "Caching egress polygons" has the full
// argument, including why the key needs no invalidation machinery.
//
// The tileset stamp rides on this struct rather than being read per chain
// because it is a property of the routing server, read once at startup. A chain
// that asked for it would pay an extra call per job to record a field nothing
// ever keys on.
//
// The zero value caches nothing, and that is a supported way to run: the cache
// is an optimisation over work the worker can always do itself, so a chainer
// built without one computes every polygon and is otherwise identical. Hence
// the logger arriving as an argument rather than a field — a zero EgressCache
// has to be usable as written.
type EgressCache struct {
	Store     store.IsochroneCache
	TilesetAt time.Time
}

// lookup reads what the cache already holds, and never reports a failure.
//
// Every way this can go wrong — no cache configured, the database unreachable,
// a row that will not parse — means the same thing to the caller: compute the
// polygon. Returning an error would invite a caller to fail a routing job over
// a missing optimisation, so the seam does not offer one; the failure goes to
// the log, where an operator can see the cache is not working.
func (e EgressCache) lookup(
	ctx context.Context,
	keys []store.IsochroneKey,
	log *logger.Logger,
) map[store.IsochroneKey]*valhalla.IsochroneResponse {
	if e.Store == nil || len(keys) == 0 {
		return nil
	}

	rows, err := e.Store.Get(ctx, keys)
	if err != nil {
		log.Printf("isochrone: egress cache unavailable, computing instead: %v", err)
		return nil
	}

	hits := make(map[store.IsochroneKey]*valhalla.IsochroneResponse, len(rows))
	for k, geometry := range rows {
		var iso valhalla.IsochroneResponse
		if err := json.Unmarshal(geometry, &iso); err != nil {
			log.Printf("isochrone: discarding unreadable cache entry %s/%s: %v", k.CompileJobID, k.StationSlug, err)
			continue
		}
		if !usable(&iso) {
			log.Printf("isochrone: discarding empty cache entry %s/%s", k.CompileJobID, k.StationSlug)
			continue
		}
		hits[k] = &iso
	}
	return hits
}

// record writes back the polygons that had to be computed, and likewise never
// fails.
//
// It runs after the fan-out has already succeeded, so by this point the answer
// the caller wanted exists. A compile job that cascaded away mid-chain, or a
// database that has gone down since, costs the next request a recomputation and
// nothing else.
//
// misses indexes keys and isos, which the chainer holds parallel to its
// candidates; only those entries were computed rather than read back.
func (e EgressCache) record(
	ctx context.Context,
	keys []store.IsochroneKey,
	isos []*valhalla.IsochroneResponse,
	misses []int,
	log *logger.Logger,
) {
	if e.Store == nil || len(misses) == 0 {
		return
	}

	entries := make([]store.CachedIsochrone, 0, len(misses))
	for _, i := range misses {
		// Nothing unusable is written, for the same reason lookup refuses to
		// serve it — and here the stakes are higher: ON CONFLICT DO NOTHING
		// means a bad row is never replaced, so caching one would turn a
		// station into a permanent miss that is recomputed on every chain
		// forever.
		if !usable(isos[i]) {
			continue
		}
		// A polygon that will not marshal is dropped rather than failing the
		// chain: the same bytes are about to be marshalled into the response,
		// and if that fails it fails there, where it is the caller's problem
		// rather than the cache's.
		geometry, err := json.Marshal(isos[i])
		if err != nil {
			log.Printf("isochrone: not caching %s: %v", keys[i].StationSlug, err)
			continue
		}
		entries = append(entries, store.CachedIsochrone{
			Key:       keys[i],
			Geometry:  geometry,
			TilesetAt: e.TilesetAt,
		})
	}

	if len(entries) == 0 {
		return
	}
	if err := e.Store.Put(ctx, entries); err != nil {
		log.Printf("isochrone: could not cache %d egress polygons: %v", len(entries), err)
	}
}

// usable reports whether a polygon is worth storing or serving.
//
// A featureless response is as good as no response — served as-is it would
// report a station as reachable with no shape to draw — so the same test guards
// both ends of the cache. Reading and writing must agree on this: a shape one
// side accepts and the other rejects is a row written once and discarded on
// every read.
func usable(iso *valhalla.IsochroneResponse) bool {
	return iso != nil && len(iso.Features) > 0
}

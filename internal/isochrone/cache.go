package isochrone

import (
	"context"
	"encoding/json"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type EgressCache struct {
	Store   store.IsochroneCache
	Stamp   *TilesetStamp
	Metrics *metrics.Set
}

func (e EgressCache) lookup(
	ctx context.Context,
	keys []store.IsochroneKey,
	log *logger.Logger,
) map[store.IsochroneKey]*valhalla.IsochroneResponse {
	if e.Store == nil || len(keys) == 0 {
		return nil
	}

	// A row is served only when it was cut from the tileset Valhalla is
	// serving now (SPA-325): a map cycle changes every polygon without
	// changing any compile job, so nothing else in the key would notice. A
	// worker that cannot name its tileset (synthetic, or /status has not
	// answered since boot) can verify no row, so it does not ask.
	current := e.Stamp.Time()
	if current.IsZero() {
		e.Metrics.Cache(metrics.ResultMiss, len(keys))
		return nil
	}

	rows, err := e.Store.Get(ctx, keys)
	if err != nil {
		log.Warn(ctx, "isochrone: egress cache unavailable, computing instead", "err", err.Error())
		e.Metrics.Cache(metrics.ResultMiss, len(keys))
		return nil
	}

	hits := make(map[store.IsochroneKey]*valhalla.IsochroneResponse, len(rows))
	stale := 0
	for k, row := range rows {
		// A NULL stamp is a miss too: a synthetic circle, or a row written before
		// /status first answered. The recompute carries a stamp, and the API's write
		// guard lets a stamped row replace an unstamped or older one, so each
		// stale row costs one recompute rather than one per request.
		if !row.TilesetAt.Equal(current) {
			stale++
			continue
		}
		var iso valhalla.IsochroneResponse
		if err := json.Unmarshal(row.Geometry, &iso); err != nil {
			log.Warn(ctx, "isochrone: discarding unreadable cache entry",
				"compile_job_id", k.CompileJobID, "station_slug", k.StationSlug, "err", err.Error())
			continue
		}
		ok, area, areaErr := usable(&iso, k)
		if !ok {
			if k.Mode == string(transit.TravelModeTransit) && len(iso.Features) > 0 {
				logRejectedTransit(ctx, log, k, area, areaErr,
					"isochrone: discarding unreadable transit cache entry",
					"isochrone: discarding walk-shaped transit cache entry")
			} else {
				log.Warn(ctx, "isochrone: discarding empty cache entry",
					"compile_job_id", k.CompileJobID, "station_slug", k.StationSlug)
			}
			continue
		}
		hits[k] = &iso
	}
	if stale > 0 {
		log.Info(ctx, "isochrone: recomputing cache entries cut from another tileset",
			"count", stale, "tileset_at", current.Format(time.RFC3339))
	}

	nHit, nMiss := 0, 0
	for _, k := range keys {
		if _, ok := hits[k]; ok {
			nHit++
		} else {
			nMiss++
		}
	}
	e.Metrics.Cache(metrics.ResultHit, nHit)
	e.Metrics.Cache(metrics.ResultMiss, nMiss)
	return hits
}

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

	tilesetAt := e.Stamp.Time()
	entries := make([]store.CachedIsochrone, 0, len(misses))
	for _, i := range misses {
		ok, area, areaErr := usable(isos[i], keys[i])
		if !ok {
			if keys[i].Mode == string(transit.TravelModeTransit) && isos[i] != nil && len(isos[i].Features) > 0 {
				logRejectedTransit(ctx, log, keys[i], area, areaErr,
					"isochrone: not caching unreadable transit polygon",
					"isochrone: not caching walk-shaped transit polygon")
			}
			continue
		}

		geometry, err := json.Marshal(isos[i])
		if err != nil {
			log.Warn(ctx, "isochrone: not caching", "station_slug", keys[i].StationSlug, "err", err.Error())
			continue
		}
		entries = append(entries, store.CachedIsochrone{
			Key:       keys[i],
			Geometry:  geometry,
			TilesetAt: tilesetAt,
		})
	}

	if len(entries) == 0 {
		return
	}
	if err := e.Store.Put(ctx, entries); err != nil {
		log.Warn(ctx, "isochrone: could not cache egress polygons", "count", len(entries), "err", err.Error())
	}
}

func usable(iso *valhalla.IsochroneResponse, key store.IsochroneKey) (bool, float64, error) {
	if iso == nil || len(iso.Features) == 0 {
		return false, 0, nil
	}
	if key.Mode != string(transit.TravelModeTransit) {
		return true, 0, nil
	}
	// A transit polygon that does not beat a walk-speed circle of the same
	// contour is either a walking answer or worthless as one. First-write-wins
	// would freeze that shape under the transit key for the rest of the service
	// day, so reject it here on both write and read.
	area, err := iso.AreaKm2()
	if err != nil {
		return false, 0, err
	}
	return area > geo.WalkCircleKm2(key.ContourMins), area, nil
}

func logRejectedTransit(ctx context.Context, log *logger.Logger, key store.IsochroneKey, area float64, areaErr error, unreadable, walkShaped string) {
	if areaErr != nil {
		log.Warn(ctx, unreadable,
			"compile_job_id", key.CompileJobID,
			"station_slug", key.StationSlug,
			"err", areaErr.Error())
		return
	}
	log.Warn(ctx, walkShaped,
		"compile_job_id", key.CompileJobID,
		"station_slug", key.StationSlug,
		"contour_mins", key.ContourMins,
		"walk_bound_km2", geo.WalkCircleKm2(key.ContourMins),
		"area_km2", area)
}

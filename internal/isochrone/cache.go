package isochrone

import (
	"context"
	"encoding/json"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type EgressCache struct {
	Store     store.IsochroneCache
	TilesetAt time.Time
}

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
		log.Warn(ctx, "isochrone: egress cache unavailable, computing instead", "err", err.Error())
		return nil
	}

	hits := make(map[store.IsochroneKey]*valhalla.IsochroneResponse, len(rows))
	for k, geometry := range rows {
		var iso valhalla.IsochroneResponse
		if err := json.Unmarshal(geometry, &iso); err != nil {
			log.Warn(ctx, "isochrone: discarding unreadable cache entry",
				"compile_job_id", k.CompileJobID, "station_slug", k.StationSlug, "err", err.Error())
			continue
		}
		if !usable(&iso) {
			log.Warn(ctx, "isochrone: discarding empty cache entry",
				"compile_job_id", k.CompileJobID, "station_slug", k.StationSlug)
			continue
		}
		hits[k] = &iso
	}
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

	entries := make([]store.CachedIsochrone, 0, len(misses))
	for _, i := range misses {
		if !usable(isos[i]) {
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
			TilesetAt: e.TilesetAt,
		})
	}

	if len(entries) == 0 {
		return
	}
	if err := e.Store.Put(ctx, entries); err != nil {
		log.Warn(ctx, "isochrone: could not cache egress polygons", "count", len(entries), "err", err.Error())
	}
}

func usable(iso *valhalla.IsochroneResponse) bool {
	return iso != nil && len(iso.Features) > 0
}

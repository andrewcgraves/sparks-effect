package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// IsochroneKey identifies one cached egress polygon.
//
// The four fields are everything that decides the shape: the compiled graph,
// whose node positions say where the polygon is centred; the station it is
// centred on; how the rider travels; and how long they have left. Two requests
// agreeing on all four get the same polygon, whatever else differs between them.
//
// The compile job is doing more work here than identifying a graph — it is what
// lets this table exist without any invalidation machinery, since a recompile
// mints a new id and rotates the whole generation on its own. README's "Caching
// egress polygons" has that argument in full.
type IsochroneKey struct {
	CompileJobID string
	StationSlug  string
	// Mode is the domain's vocabulary — walk / bike / drive / transit —
	// matching the column and the routing job the request came from.
	// Valhalla's "costing" is the same concept under a vendor name, and that
	// translation stays at the routing client's boundary.
	Mode string
	// ContourMins is the contour actually asked of Valhalla, after clamping to
	// the server's ceiling — not the rider's remaining budget. The two differ
	// only above the ceiling, where every larger budget yields the same polygon,
	// so keying on the clamped value is what lets those budgets share one row
	// instead of storing the same shape once per budget.
	ContourMins int
}

// CachedIsochrone is one row of the cache: a key, the polygon, and the tiles it
// was cut from.
type CachedIsochrone struct {
	Key IsochroneKey
	// Geometry is the routing server's answer, stored verbatim. This package
	// does not look inside one — it is JSON to Postgres and JSON to the caller,
	// and parsing a polygon here only to re-emit it would be a lossy round trip
	// for no gain.
	Geometry json.RawMessage
	// TilesetAt is when the routing tiles behind Geometry were built. It is
	// recorded as data and deliberately kept out of the key: a tile rebuild
	// changes every polygon without changing any compile job, and this column is
	// what makes "why is this polygon wrong?" answerable afterwards. Keying on
	// it would silently invalidate the entire cache on every rebuild, which is a
	// decision to take explicitly rather than by accident. A zero time means the
	// server reported none, and is stored NULL.
	TilesetAt time.Time
}

// IsochroneCache is the seam the chainer reuses egress polygons through.
//
// Both calls are batched because the chainer's access to this table is batched
// by nature: one chain looks up every reachable station at once and writes back
// everything it had to compute. A per-station interface would turn one round
// trip into as many as the scenario has stations.
//
// Both may fail, and neither failure is the caller's job to recover from. The
// cache is an optimisation over work the worker can always do itself, so a
// caller is expected to log and carry on rather than fail a routing job over
// it; see isochrone.EgressCache.
type IsochroneCache interface {
	// Get returns the entries that are present. A key with no row is simply
	// absent from the map — a miss is not an error.
	Get(ctx context.Context, keys []IsochroneKey) (map[IsochroneKey]json.RawMessage, error)
	// Put records freshly computed polygons. An entry whose key is already
	// present is left alone: the row that is there was computed from the same
	// four inputs and says the same thing.
	Put(ctx context.Context, entries []CachedIsochrone) error
}

var _ IsochroneCache = (*Repo)(nil)

// Get looks every key up in one round trip.
//
// The keys arrive as four parallel arrays unnested into a join, rather than as
// a statement built per call: a chain over a large scenario asks about as many
// stations as it found reachable, and a query whose text grows with that would
// defeat the statement cache on every differently sized request.
//
// The keys it returns are the caller's own, echoed back out of the join rather
// than read off the row. Postgres normalises a uuid on the way in, so a caller
// that spelled one with different casing would otherwise get back a key it
// could not find in its own map — a hit that reads as a miss.
func (r *Repo) Get(ctx context.Context, keys []IsochroneKey) (map[IsochroneKey]json.RawMessage, error) {
	found := make(map[IsochroneKey]json.RawMessage, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	compileJobIDs := make([]string, len(keys))
	slugs := make([]string, len(keys))
	modes := make([]string, len(keys))
	contours := make([]int32, len(keys))
	for i, k := range keys {
		compileJobIDs[i] = k.CompileJobID
		slugs[i] = k.StationSlug
		modes[i] = k.Mode
		contours[i] = int32(k.ContourMins)
	}

	rows, err := r.pool.Query(ctx,
		`SELECT wanted.compile_job_id, wanted.station_slug, wanted.mode, wanted.contour_mins, c.geometry
		   FROM unnest($1::text[], $2::text[], $3::text[], $4::int[])
		        AS wanted (compile_job_id, station_slug, mode, contour_mins)
		   JOIN isochrone_cache c
		     ON c.compile_job_id = wanted.compile_job_id::uuid
		    AND c.station_slug   = wanted.station_slug
		    AND c.mode           = wanted.mode
		    AND c.contour_mins   = wanted.contour_mins`,
		compileJobIDs, slugs, modes, contours)
	if err != nil {
		return nil, fmt.Errorf("store: reading isochrone cache: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			k    IsochroneKey
			geom []byte
		)
		if err := rows.Scan(&k.CompileJobID, &k.StationSlug, &k.Mode, &k.ContourMins, &geom); err != nil {
			return nil, fmt.Errorf("store: scanning isochrone cache row: %w", err)
		}
		found[k] = json.RawMessage(geom)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading isochrone cache: %w", err)
	}
	return found, nil
}

// Put writes the batch in one round trip.
//
// DO NOTHING rather than DO UPDATE: a row is only ever written after a miss, so
// a conflict means another worker chained over the same graph concurrently and
// computed the same four inputs into the same polygon. Overwriting it would
// churn the row to no effect.
func (r *Repo) Put(ctx context.Context, entries []CachedIsochrone) error {
	if len(entries) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, e := range entries {
		// Nil rather than the zero time when no tileset was reported: the
		// column is nullable precisely so "unknown" is distinguishable from
		// "built at the epoch".
		var tilesetAt *time.Time
		if !e.TilesetAt.IsZero() {
			at := e.TilesetAt
			tilesetAt = &at
		}
		batch.Queue(
			`INSERT INTO isochrone_cache
			     (compile_job_id, station_slug, mode, contour_mins, geometry, tileset_at)
			  VALUES ($1::uuid, $2, $3, $4, $5, $6)
			  ON CONFLICT (compile_job_id, station_slug, mode, contour_mins) DO NOTHING`,
			e.Key.CompileJobID, e.Key.StationSlug, e.Key.Mode, e.Key.ContourMins,
			[]byte(e.Geometry), tilesetAt)
	}

	if err := r.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store: writing isochrone cache: %w", err)
	}
	return nil
}

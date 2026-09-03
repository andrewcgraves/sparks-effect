// Package store is this worker's half of the routing_jobs table.
//
// The table is co-owned by construction: sparks-effect-api inserts a row and
// polls it, this worker transitions it and writes its result. The schema —
// including this table — lives in that repository's migration sequence, because
// both it and the isochrone cache want real foreign keys into users and jobs,
// and a foreign key cannot cross a schema owned by a different migration
// sequence.
//
// This worker does not connect to Postgres. Exposing the database past the
// API's private network is the thing SPA-273 exists to stop, so every write
// goes through authenticated HTTP endpoints the API owns. The SQL that used
// to live in this package lives there now; what remains here is the seam the
// rest of the worker is written against, and the client that speaks the
// contract. There are still no migrations here, and there deliberately never
// will be.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Job status values, matching the vocabulary sparks-effect-api uses for both
// compile jobs and routing jobs. Reusing it means a client polling either
// surface does not have to learn two spellings of the same lifecycle.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ErrJobNotFound reports an update that matched no row.
//
// It is not necessarily an error in the worker: a routing job whose compile job
// was deleted cascades away, so a message can outlive its row. The consumer
// treats it as permanent — there is nothing to write the answer to — and acks
// rather than redelivering forever. The API signals this with HTTP 404.
var ErrJobNotFound = errors.New("store: routing job not found")

// RoutingJobStore is the seam the worker writes results through.
//
// Three calls, one per transition the worker is responsible for. There is no
// read: everything needed to compute the isochrone arrives on the message, so a
// worker that could not reach the API would still have nowhere useful to put
// an answer.
type RoutingJobStore interface {
	MarkRunning(ctx context.Context, jobID string) error
	MarkSucceeded(ctx context.Context, jobID string, result json.RawMessage) error
	MarkFailed(ctx context.Context, jobID, message string) error
}

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
	CompileJobID string `json:"compile_job_id"`
	StationSlug  string `json:"station_slug"`
	// Mode is the domain's vocabulary — walk / bike / drive / transit —
	// matching the column and the routing job the request came from.
	// Valhalla's "costing" is the same concept under a vendor name, and that
	// translation stays at the routing client's boundary.
	Mode string `json:"mode"`
	// ContourMins is the contour actually asked of Valhalla, after clamping to
	// the server's ceiling — not the rider's remaining budget. The two differ
	// only above the ceiling, where every larger budget yields the same polygon,
	// so keying on the clamped value is what lets those budgets share one row
	// instead of storing the same shape once per budget.
	ContourMins int `json:"contour_mins"`
}

// CachedIsochrone is one row of the cache: a key, the polygon, and the tiles it
// was cut from.
type CachedIsochrone struct {
	Key IsochroneKey `json:"key"`
	// Geometry is the routing server's answer, stored verbatim. This package
	// does not look inside one — it is JSON to the API and JSON to the caller,
	// and parsing a polygon here only to re-emit it would be a lossy round trip
	// for no gain.
	Geometry json.RawMessage `json:"geometry"`
	// TilesetAt is when the routing tiles behind Geometry were built. It is
	// recorded as data and deliberately kept out of the key: a tile rebuild
	// changes every polygon without changing any compile job, and this column is
	// what makes "why is this polygon wrong?" answerable afterwards. Keying on
	// it would silently invalidate the entire cache on every rebuild, which is a
	// decision to take explicitly rather than by accident. A zero time means the
	// server reported none, and is stored NULL.
	TilesetAt time.Time `json:"tileset_at,omitempty"`
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

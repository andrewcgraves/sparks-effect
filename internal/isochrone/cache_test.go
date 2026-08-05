package isochrone_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// fakeCache is an in-memory store.IsochroneCache that remembers what it was
// asked and what it was told.
//
// It is a real cache rather than a pair of canned answers because most of these
// cases are about the *second* chain over the same graph — what the first one
// wrote is exactly what decides whether the second one calls Valhalla.
type fakeCache struct {
	mu      sync.Mutex
	entries map[store.IsochroneKey]json.RawMessage
	getErr  error
	putErr  error

	getKeys []store.IsochroneKey
	puts    []store.CachedIsochrone
}

var _ store.IsochroneCache = (*fakeCache)(nil)

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[store.IsochroneKey]json.RawMessage)}
}

func (f *fakeCache) Get(_ context.Context, keys []store.IsochroneKey) (map[store.IsochroneKey]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getKeys = append(f.getKeys, keys...)
	if f.getErr != nil {
		return nil, f.getErr
	}
	hits := make(map[store.IsochroneKey]json.RawMessage)
	for _, k := range keys {
		if v, ok := f.entries[k]; ok {
			hits[k] = v
		}
	}
	return hits, nil
}

func (f *fakeCache) Put(_ context.Context, entries []store.CachedIsochrone) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, entries...)
	if f.putErr != nil {
		return f.putErr
	}
	for _, e := range entries {
		f.entries[e.Key] = e.Geometry
	}
	return nil
}

// written returns a copy of everything the cache was told to store.
func (f *fakeCache) written() []store.CachedIsochrone {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.CachedIsochrone(nil), f.puts...)
}

// seed plants an entry directly, standing in for a row an earlier chain left.
func (f *fakeCache) seed(k store.IsochroneKey, geometry json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[k] = geometry
}

// viaTransitGraph is the fixture behind most of these cases: a hub directly
// reachable on foot, with station-a and station-b reachable only by riding a
// service from it.
//
// The hub itself, not either station, is what a rider actually walks to — and
// since SPA-188 a station reached without boarding anything gets no egress
// polygon and nothing to cache. A fixture built on two directly-walkable
// stations, which is what this used to be, no longer exercises the cache at
// all: both would resolve as walk-only and neither would produce a polygon
// worth reusing.
func viaTransitGraph() *fakeGraph {
	return &fakeGraph{
		nodes: []transit.Node{
			node("hub", -122.40, 37.70),
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
		travelTimes: map[[2]string]int{
			{"hub", "station-a"}: 60,
			{"hub", "station-b"}: 300,
		},
	}
}

// twoReachableStations is the fixture behind most of these cases: the hub is
// directly walkable, station-a and station-b are not — so a cold chain makes
// three isochrone calls (the origin, plus one per station reached by transit)
// and a fully warm one makes a single call.
func twoReachableStations() *valhalla.StubClient {
	return &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					{Time: 600, Distance: 1.0}, // hub: 10 mins, directly walkable
					{Time: -1, Distance: 0},    // station-a: only reachable via the hub
					{Time: -1, Distance: 0},    // station-b: only reachable via the hub
				},
			},
		},
	}
}

func newCachingChainer(client valhalla.Client, graph transit.GraphData, cache isochrone.EgressCache) isochrone.Chainer {
	return isochrone.New(client, graph, valhalla.DefaultLimits(), cache, logger.Discard())
}

// isochroneCallCount reports how many isochrone requests a stub was given.
func isochroneCallCount(t *testing.T, fc *valhalla.StubClient) int {
	t.Helper()
	iso, _ := fc.Calls()
	return len(iso)
}

// The headline acceptance criterion: a warm cache measurably reduces the number
// of Valhalla calls, asserted rather than observed.
//
// Two chains over one cache. The first pays for everything; the second pays
// only for the origin polygon, which is the one call that can never be reused
// because it starts where the user clicked.
func TestChainer_warmCacheReducesRoutingCalls(t *testing.T) {
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	cold := twoReachableStations()
	coldResp, err := newCachingChainer(cold, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("cold Chain: %v", err)
	}
	coldCalls := isochroneCallCount(t, cold)
	if coldCalls != 3 {
		t.Fatalf("cold chain made %d isochrone calls, want 3 (origin + 2 stations)", coldCalls)
	}

	warm := twoReachableStations()
	warmResp, err := newCachingChainer(warm, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("warm Chain: %v", err)
	}
	warmCalls := isochroneCallCount(t, warm)

	if warmCalls >= coldCalls {
		t.Errorf("warm chain made %d isochrone calls against a cold %d; the cache saved nothing", warmCalls, coldCalls)
	}
	if warmCalls != 1 {
		t.Errorf("warm chain made %d isochrone calls, want 1 (the origin, which is never cacheable)", warmCalls)
	}

	// A cheaper answer is only worth anything if it is the same answer.
	if len(warmResp.Features) != len(coldResp.Features) {
		t.Errorf("warm chain returned %d features, cold returned %d", len(warmResp.Features), len(coldResp.Features))
	}
	if len(warmResp.Metadata.ReachableStations) != len(coldResp.Metadata.ReachableStations) {
		t.Errorf("warm chain reported %d reachable stations, cold reported %d",
			len(warmResp.Metadata.ReachableStations), len(coldResp.Metadata.ReachableStations))
	}
}

// The one call a warm chain still makes must be the origin one. A cache that
// served the origin polygon from a station's row would draw the surface in the
// wrong place.
func TestChainer_originIsochroneIsNeverCached(t *testing.T) {
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	cold := twoReachableStations()
	if _, err := newCachingChainer(cold, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("cold Chain: %v", err)
	}

	warm := twoReachableStations()
	if _, err := newCachingChainer(warm, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("warm Chain: %v", err)
	}

	iso, _ := warm.Calls()
	if len(iso) != 1 {
		t.Fatalf("warm chain made %d isochrone calls, want 1", len(iso))
	}
	if iso[0].Origin.Lat != 37.7 || iso[0].Origin.Lng != -122.4 {
		t.Errorf("the surviving call was from %v, want the request origin (37.7, -122.4)", iso[0].Origin)
	}

	// Nothing centred on the click should ever have been offered to the cache.
	for _, e := range cache.written() {
		if e.Key.StationSlug == "" {
			t.Error("an entry with no station slug was written; the origin polygon must not be cached")
		}
	}
}

// The key is (compile_job_id, station_slug, mode, contour_mins), in the
// domain's vocabulary — not Valhalla's costing.
func TestChainer_cacheKeyIsCompileStationModeAndContour(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeBike)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) != 2 {
		t.Fatalf("cached %d polygons, want 2 (one per reachable station)", len(written))
	}

	// station-a: 90 - 10 access - 1 transit = 79 left. station-b: 90 - 10 - 5 = 75.
	want := map[string]int{"station-a": 79, "station-b": 75}
	for _, e := range written {
		contour, known := want[e.Key.StationSlug]
		if !known {
			t.Errorf("cached an unexpected station %q", e.Key.StationSlug)
			continue
		}
		if e.Key.CompileJobID != "compile-1" {
			t.Errorf("%s: compile job = %q, want compile-1", e.Key.StationSlug, e.Key.CompileJobID)
		}
		if e.Key.Mode != "bike" {
			t.Errorf("%s: mode = %q, want the domain's bike rather than Valhalla's bicycle", e.Key.StationSlug, e.Key.Mode)
		}
		if e.Key.ContourMins != contour {
			t.Errorf("%s: contour = %d mins, want %d", e.Key.StationSlug, e.Key.ContourMins, contour)
		}
		delete(want, e.Key.StationSlug)
	}
	for slug := range want {
		t.Errorf("%s was never cached", slug)
	}
}

// The stamp is recorded on every row and is not part of what is looked up. The
// second half of that is the store's to enforce — the key type has no such
// field — so what is checked here is that the chainer actually supplies it.
func TestChainer_cachedEntriesCarryTheTilesetStamp(t *testing.T) {
	built := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cache := newFakeCache()
	fc := twoReachableStations()
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache, TilesetAt: built})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) == 0 {
		t.Fatal("nothing was cached")
	}
	for _, e := range written {
		if !e.TilesetAt.Equal(built) {
			t.Errorf("%s: tileset stamp = %s, want %s", e.Key.StationSlug, e.TilesetAt, built)
		}
	}
}

// A recompile mints a new compile job, and the previous generation's rows are
// simply not addressable from it. The store proves this against Postgres; this
// proves the chainer puts the request's own compile job in the key rather than
// anything it inferred.
func TestChainer_recompiledGraphMissesThePreviousGeneration(t *testing.T) {
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	first := twoReachableStations()
	if _, err := newCachingChainer(first, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("first Chain: %v", err)
	}

	recompiled := req(90, transit.TravelModeWalk)
	recompiled.CompileJobID = "compile-2"

	second := twoReachableStations()
	if _, err := newCachingChainer(second, viaTransitGraph(), egress).
		Chain(context.Background(), recompiled); err != nil {
		t.Fatalf("second Chain: %v", err)
	}

	if got := isochroneCallCount(t, second); got != 3 {
		t.Errorf("the recompiled graph made %d isochrone calls, want 3 — it must not read the previous generation", got)
	}
}

// Above the contour ceiling every remaining budget yields the same polygon, so
// the key carries the clamped contour and those budgets share one row.
func TestChainer_cacheKeyUsesTheClampedContour(t *testing.T) {
	const ceilingMins = 40

	limits := valhalla.DefaultLimits()
	limits.MaxIsoContourMins = ceilingMins
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	// 90 - 10 - 1 = 79 minutes left at station-a, clamped to the 40-minute ceiling.
	first := twoReachableStations()
	if _, err := isochrone.New(first, viaTransitGraph(), limits, egress, logger.Discard()).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("first Chain: %v", err)
	}

	for _, e := range cache.written() {
		if e.Key.ContourMins != ceilingMins {
			t.Errorf("%s: cached contour = %d, want the %d-minute ceiling the call actually asked for",
				e.Key.StationSlug, e.Key.ContourMins, ceilingMins)
		}
	}

	// A larger budget clamps to the same contour, so it should hit the rows the
	// smaller one wrote rather than compute its own.
	larger := twoReachableStations()
	if _, err := isochrone.New(larger, viaTransitGraph(), limits, egress, logger.Discard()).
		Chain(context.Background(), req(120, transit.TravelModeWalk)); err != nil {
		t.Fatalf("second Chain: %v", err)
	}
	if got := isochroneCallCount(t, larger); got != 1 {
		t.Errorf("a larger budget clamping to the same contour made %d isochrone calls, want 1 (origin only)", got)
	}
}

// A cache that cannot be reached is not a failed job. The chain computes
// everything and answers exactly as it would with no cache configured.
func TestChainer_unavailableCacheDegradesToComputing(t *testing.T) {
	cache := newFakeCache()
	cache.getErr = errors.New("connection refused")
	fc := twoReachableStations()
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Features) != 3 {
		t.Errorf("Features = %d, want 3", len(resp.Features))
	}
	if got := isochroneCallCount(t, fc); got != 3 {
		t.Errorf("made %d isochrone calls, want 3 — an unreachable cache means compute everything", got)
	}
}

// Nor is a cache that cannot be written to. By the time the write is attempted
// the answer already exists; losing it costs the next request a recomputation.
func TestChainer_uncacheableResultDoesNotFailTheJob(t *testing.T) {
	cache := newFakeCache()
	cache.putErr = errors.New("compile job cascaded away")
	fc := twoReachableStations()
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Features) != 3 {
		t.Errorf("Features = %d, want 3", len(resp.Features))
	}
	// The hub, station-a and station-b: the hub is walk-only and carries no
	// polygon, but it is still a station the rider reached.
	if len(resp.Metadata.ReachableStations) != 3 {
		t.Errorf("ReachableStations = %d, want 3", len(resp.Metadata.ReachableStations))
	}
}

// An entry that will not parse, and one that parses to nothing, are both
// recomputed rather than served. The second is the more dangerous of the two:
// served as-is it would report a station as reachable with no shape to draw.
func TestChainer_corruptEntryIsRecomputed(t *testing.T) {
	for name, geometry := range map[string]json.RawMessage{
		"unparseable": json.RawMessage(`{"type":"FeatureColl`),
		"no features": json.RawMessage(`{"type":"FeatureCollection","features":[]}`),
		"null":        json.RawMessage(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			cache := newFakeCache()
			// station-a corrupt, station-b sound: only the corrupt one should
			// cost a call.
			cache.seed(store.IsochroneKey{
				CompileJobID: "compile-1", StationSlug: "station-a", Mode: "walk", ContourMins: 79,
			}, geometry)
			sound, err := json.Marshal(cannedIso())
			if err != nil {
				t.Fatalf("marshalling fixture: %v", err)
			}
			cache.seed(store.IsochroneKey{
				CompileJobID: "compile-1", StationSlug: "station-b", Mode: "walk", ContourMins: 75,
			}, sound)

			fc := twoReachableStations()
			chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

			resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
			if err != nil {
				t.Fatalf("Chain: %v", err)
			}
			if len(resp.Features) != 3 {
				t.Errorf("Features = %d, want 3 — a corrupt entry must still yield a polygon", len(resp.Features))
			}
			// Origin, plus station-a recomputed. station-b came from the cache.
			if got := isochroneCallCount(t, fc); got != 2 {
				t.Errorf("made %d isochrone calls, want 2 (origin + the corrupt station)", got)
			}
		})
	}
}

// The other end of the corruption guard: what a lookup would refuse to serve
// must never be written in the first place.
//
// The two ends have to agree, and this end matters more. A write is
// ON CONFLICT DO NOTHING, so a featureless row is stored once and never
// replaced — the station would be recomputed and discarded on every chain from
// then on, a permanent miss that repairs itself only if someone deletes the row.
func TestChainer_featurelessPolygonIsNotCached(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.71 { // station-a
			return &valhalla.IsochroneResponse{Type: "FeatureCollection"}, nil
		}
		return cannedIso(), nil
	}
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	var cachedB bool
	for _, e := range cache.written() {
		switch e.Key.StationSlug {
		case "station-a":
			t.Error("a featureless polygon was cached; it would be discarded on every read and never replaced")
		case "station-b":
			cachedB = true
		}
	}
	// The sound polygon alongside it is still cached — one bad answer must not
	// cost the whole batch.
	if !cachedB {
		t.Error("station-b's polygon was not cached")
	}
}

// A chainer built without a cache is a supported configuration, not a crash.
func TestChainer_zeroCacheIsSupported(t *testing.T) {
	fc := twoReachableStations()
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{})

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Features) != 3 {
		t.Errorf("Features = %d, want 3", len(resp.Features))
	}
	if got := isochroneCallCount(t, fc); got != 3 {
		t.Errorf("made %d isochrone calls, want 3", got)
	}
}

// A chain that reaches nothing has nothing to look up, and should not go to the
// database to say so.
func TestChainer_noReachableStationsTouchesNoCache(t *testing.T) {
	cache := newFakeCache()
	fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
	chainer := newCachingChainer(fc, &fakeGraph{}, isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.getKeys) != 0 {
		t.Errorf("looked up %d keys for a graph with no reachable stations, want 0", len(cache.getKeys))
	}
	if len(cache.puts) != 0 {
		t.Errorf("wrote %d entries for a graph with no reachable stations, want 0", len(cache.puts))
	}
}

// A fully warm chain has nothing new to write, and should not open a write for
// the sake of it.
func TestChainer_fullyWarmChainWritesNothing(t *testing.T) {
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("cold Chain: %v", err)
	}
	writesAfterCold := len(cache.written())

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("warm Chain: %v", err)
	}
	if got := len(cache.written()); got != writesAfterCold {
		t.Errorf("a fully warm chain wrote %d further entries, want 0", got-writesAfterCold)
	}
}

// A failed fan-out must not leave the polygons it did compute in the cache
// under keys the failed request never completed — but more importantly, the
// error still has to reach the caller.
func TestChainer_fanOutFailureStillFailsTheChain(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.71 { // station-a
			return nil, valhalla.ErrBadRequest
		}
		return cannedIso(), nil
	}
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err == nil {
		t.Fatal("Chain succeeded despite a rejected egress request")
	}
	if got := len(cache.written()); got != 0 {
		t.Errorf("a failed chain wrote %d cache entries, want 0", got)
	}
}

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
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

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

func (f *fakeCache) written() []store.CachedIsochrone {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.CachedIsochrone(nil), f.puts...)
}

func (f *fakeCache) seed(k store.IsochroneKey, geometry json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[k] = geometry
}

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

func twoReachableStations() *valhalla.StubClient {
	return &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					valhalla.Cell(600, tokenRoadKm),
					{},
					{},
				},
			},
		},
	}
}

func twoStationsByTransit(iso *valhalla.IsochroneResponse) *valhalla.StubClient {
	return &valhalla.StubClient{
		IsochroneResp: iso,
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			if r.Destination.Lat == 37.70 && r.Destination.Lng == -122.40 {
				return routedAccess(600), nil
			}
			return nil, valhalla.ErrBadRequest
		},
	}
}

func newCachingChainer(client valhalla.Client, graph transit.GraphData, cache isochrone.EgressCache) isochrone.Chainer {
	return isochrone.New(client, graph, valhalla.DefaultLimits(), cache, logger.Discard(), nil)
}

func isochroneCallCount(t *testing.T, fc *valhalla.StubClient) int {
	t.Helper()
	iso, _ := fc.Calls()
	return len(iso)
}

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

	if len(warmResp.Features) != len(coldResp.Features) {
		t.Errorf("warm chain returned %d features, cold returned %d", len(warmResp.Features), len(coldResp.Features))
	}
	if len(warmResp.Metadata.ReachableStations) != len(coldResp.Metadata.ReachableStations) {
		t.Errorf("warm chain reported %d reachable stations, cold reported %d",
			len(warmResp.Metadata.ReachableStations), len(coldResp.Metadata.ReachableStations))
	}
}

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

	for _, e := range cache.written() {
		if e.Key.StationSlug == "" {
			t.Error("an entry with no station slug was written; the origin polygon must not be cached")
		}
	}
}

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

func TestChainer_cacheKeyTransitIsTheDomainWordNotMultimodal(t *testing.T) {
	cache := newFakeCache()
	fc := twoStationsByTransit(transitShapedIso())
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) != 2 {
		t.Fatalf("cached %d polygons, want 2 (one per station reached by riding)", len(written))
	}
	for _, e := range written {
		if e.Key.Mode != "transit" {
			t.Errorf("%s: mode = %q, want the domain's transit rather than Valhalla's multimodal", e.Key.StationSlug, e.Key.Mode)
		}
	}
	_, matrix := fc.Calls()
	if len(matrix) != 0 {
		t.Errorf("transit cache fill called Matrix %d times", len(matrix))
	}
}

func TestChainer_cachedEntriesCarryTheTilesetStamp(t *testing.T) {
	built := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cache := newFakeCache()
	fc := twoReachableStations()
	chainer := newCachingChainer(fc, viaTransitGraph(),
		isochrone.EgressCache{Store: cache, Stamp: isochrone.NewTilesetStamp(built)})

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

func TestChainer_cacheKeyUsesTheClampedContour(t *testing.T) {
	const ceilingMins = 40

	limits := valhalla.DefaultLimits()
	limits.MaxIsoContourMins = ceilingMins
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	first := twoReachableStations()
	if _, err := isochrone.New(first, viaTransitGraph(), limits, egress, logger.Discard(), nil).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("first Chain: %v", err)
	}

	for _, e := range cache.written() {
		if e.Key.ContourMins != ceilingMins {
			t.Errorf("%s: cached contour = %d, want the %d-minute ceiling the call actually asked for",
				e.Key.StationSlug, e.Key.ContourMins, ceilingMins)
		}
	}

	larger := twoReachableStations()
	if _, err := isochrone.New(larger, viaTransitGraph(), limits, egress, logger.Discard(), nil).
		Chain(context.Background(), req(120, transit.TravelModeWalk)); err != nil {
		t.Fatalf("second Chain: %v", err)
	}
	if got := isochroneCallCount(t, larger); got != 1 {
		t.Errorf("a larger budget clamping to the same contour made %d isochrone calls, want 1 (origin only)", got)
	}
}

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

	if len(resp.Metadata.ReachableStations) != 3 {
		t.Errorf("ReachableStations = %d, want 3", len(resp.Metadata.ReachableStations))
	}
}

func TestChainer_corruptEntryIsRecomputed(t *testing.T) {
	for name, geometry := range map[string]json.RawMessage{
		"unparseable": json.RawMessage(`{"type":"FeatureColl`),
		"no features": json.RawMessage(`{"type":"FeatureCollection","features":[]}`),
		"null":        json.RawMessage(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			cache := newFakeCache()

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

			if got := isochroneCallCount(t, fc); got != 2 {
				t.Errorf("made %d isochrone calls, want 2 (origin + the corrupt station)", got)
			}
		})
	}
}

func TestChainer_featurelessPolygonIsNotCached(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.71 {
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

	if !cachedB {
		t.Error("station-b's polygon was not cached")
	}
}

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

func TestChainer_fanOutDropDoesNotCacheTheFailedStation(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.71 {
			return nil, valhalla.ErrBadRequest
		}
		return cannedIso(), nil
	}
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v, want the chain to survive a per-station egress 400", err)
	}

	var cachedA, cachedB bool
	for _, e := range cache.written() {
		switch e.Key.StationSlug {
		case "station-a":
			cachedA = true
		case "station-b":
			cachedB = true
		}
	}
	if cachedA {
		t.Error("station-a was cached after its egress call failed")
	}
	if !cachedB {
		t.Error("station-b's polygon was not cached")
	}
}

func TestChainer_rateLimitAbortStillCachesTheSuccessfulStation(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.69 {
			return nil, valhalla.ErrRateLimit
		}
		return cannedIso(), nil
	}
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	_, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if !errors.Is(err, isochrone.ErrRoutingRateLimit) {
		t.Errorf("err = %v, want ErrRoutingRateLimit", err)
	}

	var cachedA, cachedB bool
	for _, e := range cache.written() {
		switch e.Key.StationSlug {
		case "station-a":
			cachedA = true
		case "station-b":
			cachedB = true
		}
	}
	if !cachedA {
		t.Error("station-a's polygon was not cached; a rate-limit retry should not re-hit Valhalla for a station that already drew")
	}
	if cachedB {
		t.Error("station-b was cached after its egress call was rate-limited")
	}
}

func TestChainer_cacheLookupsAreCounted(t *testing.T) {
	cache := newFakeCache()
	m := &metrics.Set{}
	egress := isochrone.EgressCache{Store: cache, Metrics: m}

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("cold Chain: %v", err)
	}
	if got := m.CacheCount(metrics.ResultHit); got != 0 {
		t.Errorf("cold hits = %d, want 0", got)
	}
	if got := m.CacheCount(metrics.ResultMiss); got != 2 {
		t.Errorf("cold misses = %d, want 2", got)
	}

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("warm Chain: %v", err)
	}
	if got := m.CacheCount(metrics.ResultHit); got != 2 {
		t.Errorf("warm hits = %d, want 2", got)
	}
	if got := m.CacheCount(metrics.ResultMiss); got != 2 {
		t.Errorf("warm misses = %d, want 2 (the cold misses still counted)", got)
	}
}

func TestChainer_unavailableCacheCountsAsMisses(t *testing.T) {
	cache := newFakeCache()
	cache.getErr = errors.New("connection refused")
	m := &metrics.Set{}

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), isochrone.EgressCache{Store: cache, Metrics: m}).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if got := m.CacheCount(metrics.ResultMiss); got != 2 {
		t.Errorf("misses = %d, want 2", got)
	}
}

func TestChainer_zeroCacheDoesNotCountLookups(t *testing.T) {
	m := &metrics.Set{}

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), isochrone.EgressCache{Metrics: m}).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if got := m.CacheCount(metrics.ResultHit); got != 0 {
		t.Errorf("hits = %d, want 0", got)
	}
	if got := m.CacheCount(metrics.ResultMiss); got != 0 {
		t.Errorf("misses = %d, want 0 — no cache is not a miss", got)
	}
}

func TestChainer_corruptEntryCountsAsAMiss(t *testing.T) {
	cache := newFakeCache()
	cache.seed(store.IsochroneKey{
		CompileJobID: "compile-1", StationSlug: "station-a", Mode: "walk", ContourMins: 79,
	}, json.RawMessage(`{"type":"FeatureColl`))
	sound, err := json.Marshal(cannedIso())
	if err != nil {
		t.Fatalf("marshalling fixture: %v", err)
	}
	cache.seed(store.IsochroneKey{
		CompileJobID: "compile-1", StationSlug: "station-b", Mode: "walk", ContourMins: 75,
	}, sound)

	m := &metrics.Set{}
	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), isochrone.EgressCache{Store: cache, Metrics: m}).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if got := m.CacheCount(metrics.ResultHit); got != 1 {
		t.Errorf("hits = %d, want 1 (the readable station)", got)
	}
	if got := m.CacheCount(metrics.ResultMiss); got != 1 {
		t.Errorf("misses = %d, want 1 (the corrupt station)", got)
	}
}

func TestChainer_walkShapedTransitPolygonIsNotCached(t *testing.T) {
	for name, iso := range map[string]*valhalla.IsochroneResponse{
		"walk-sized circle": walkShapedIso(),
		"empty coordinates": cannedIso(),
	} {
		t.Run(name, func(t *testing.T) {
			cache := newFakeCache()
			fc := twoStationsByTransit(iso)
			chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

			resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit))
			if err != nil {
				t.Fatalf("Chain: %v", err)
			}
			if len(resp.Metadata.ReachableStations) == 0 {
				t.Fatal("reachable stations empty; the chain must still report the stations")
			}
			if len(resp.Features) < 2 {
				t.Fatal("features < 2; a rejected cache write must still return the egress polygon, not just the origin")
			}
			var hasStationSlug bool
			for _, f := range resp.Features {
				var feat map[string]any
				if err := json.Unmarshal(f, &feat); err != nil {
					t.Fatalf("unmarshal feature: %v", err)
				}
				props, _ := feat["properties"].(map[string]any)
				if _, ok := props["station_slug"]; ok {
					hasStationSlug = true
					break
				}
			}
			if !hasStationSlug {
				t.Error("no feature has properties.station_slug; a rejected write must still return the injected egress polygon")
			}
			if got := len(cache.written()); got != 0 {
				t.Errorf("cached %d polygons, want 0 — a walk-shaped transit surface must not be stored", got)
			}
		})
	}
}

func TestChainer_genuineTransitPolygonIsCached(t *testing.T) {
	cache := newFakeCache()
	fc := twoStationsByTransit(transitShapedIso())
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) != 2 {
		t.Fatalf("cached %d polygons, want 2", len(written))
	}
	for _, e := range written {
		if e.Key.Mode != string(transit.TravelModeTransit) {
			t.Errorf("%s: mode = %q, want transit", e.Key.StationSlug, e.Key.Mode)
		}
	}
}

func TestChainer_walkModeSmallPolygonStillCaches(t *testing.T) {
	cache := newFakeCache()
	fc := twoReachableStations()
	fc.IsochroneResp = walkShapedIso()
	chainer := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	if got := len(cache.written()); got != 2 {
		t.Errorf("cached %d polygons, want 2 — the area guard is transit-only", got)
	}
}

func TestChainer_poisonedWalkShapedTransitIsDiscardedOnLookup(t *testing.T) {
	at := departure(t)
	walkGeom, err := json.Marshal(walkShapedIso())
	if err != nil {
		t.Fatalf("marshalling walk fixture: %v", err)
	}
	transitGeom, err := json.Marshal(transitShapedIso())
	if err != nil {
		t.Fatalf("marshalling transit fixture: %v", err)
	}

	cache := newFakeCache()
	cache.seed(store.IsochroneKey{
		CompileJobID: "compile-1", StationSlug: "station-a", Mode: "transit", ContourMins: 79, DepartsOn: "2026-09-02",
	}, walkGeom)
	cache.seed(store.IsochroneKey{
		CompileJobID: "compile-1", StationSlug: "station-b", Mode: "transit", ContourMins: 75, DepartsOn: "2026-09-02",
	}, transitGeom)

	cold := twoStationsByTransit(walkShapedIso())
	if _, err := newCachingChainer(cold, viaTransitGraph(), isochrone.EgressCache{Store: cache}).
		Chain(context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	if got := isochroneCallCount(t, cold); got != 2 {
		t.Errorf("made %d isochrone calls, want 2 (origin + station-a) — station-b is a genuine hit", got)
	}
	for _, e := range cache.written() {
		if e.Key.StationSlug == "station-a" {
			t.Error("wrote station-a after recomputing a walk-shaped answer")
		}
	}

	warm := twoStationsByTransit(walkShapedIso())
	if _, err := newCachingChainer(warm, viaTransitGraph(), isochrone.EgressCache{Store: cache}).
		Chain(context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("second Chain: %v", err)
	}
	if got := isochroneCallCount(t, warm); got != 2 {
		t.Errorf("second chain made %d isochrone calls, want 2 — walk-shaped transit must not stick", got)
	}
	for _, e := range cache.written() {
		if e.Key.StationSlug == "station-a" {
			t.Error("second chain wrote station-a")
		}
	}
}

func TestChainer_genuineTransitRoundTripsThroughCache(t *testing.T) {
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	cold := twoStationsByTransit(transitShapedIso())
	if _, err := newCachingChainer(cold, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("cold Chain: %v", err)
	}
	coldCalls := isochroneCallCount(t, cold)
	if coldCalls != 3 {
		t.Fatalf("cold chain made %d isochrone calls, want 3 (origin + 2 stations)", coldCalls)
	}
	if got := len(cache.written()); got != 2 {
		t.Fatalf("cached %d polygons, want 2", got)
	}

	warm := twoStationsByTransit(transitShapedIso())
	if _, err := newCachingChainer(warm, viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("warm Chain: %v", err)
	}
	if got := isochroneCallCount(t, warm); got != 1 {
		t.Errorf("warm chain made %d isochrone calls, want 1 (the origin)", got)
	}
	if got := len(cache.written()); got != 2 {
		t.Errorf("warm chain wrote further entries; cache holds %d writes, want 2", got)
	}
}

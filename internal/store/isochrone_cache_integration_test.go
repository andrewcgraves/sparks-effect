package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/testsupport"
)

// Two compile jobs standing for two publications of the same scenario: an
// edit produces a new compile, and with it a new cache generation.
const (
	firstCompileID  = "aa11bb22-cc33-dd44-ee55-ff6677889900"
	secondCompileID = "bb22cc33-dd44-ee55-ff66-778899001122"
)

func key(compileJobID, slug string, contourMins int) store.IsochroneKey {
	return store.IsochroneKey{
		CompileJobID: compileJobID,
		StationSlug:  slug,
		Mode:         "walk",
		ContourMins:  contourMins,
	}
}

func polygon(slug string) json.RawMessage {
	return json.RawMessage(`{"type":"FeatureCollection","features":[{"slug":"` + slug + `"}]}`)
}

func TestIsochroneCache_roundTrip(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	stored := key(firstCompileID, "station-a", 30)
	missing := key(firstCompileID, "station-b", 30)

	if err := repo.Put(ctx, []store.CachedIsochrone{
		{Key: stored, Geometry: polygon("station-a"), TilesetAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := repo.Get(ctx, []store.IsochroneKey{stored, missing})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Get returned %d entries, want 1 (the other key was never stored)", len(got))
	}
	var round map[string]any
	if err := json.Unmarshal(got[stored], &round); err != nil {
		t.Fatalf("cached geometry is not valid JSON: %v", err)
	}
	if round["type"] != "FeatureCollection" {
		t.Errorf("geometry type = %v, want FeatureCollection", round["type"])
	}
	if _, ok := got[missing]; ok {
		t.Error("Get returned an entry for a key that was never stored")
	}
}

// Every field of the key has to matter, or a chain would be served a polygon
// centred on the wrong station, cut for the wrong mode, or sized for the wrong
// budget. Each case below stores one row and asks for it with exactly one field
// changed.
func TestIsochroneCache_everyKeyFieldDiscriminates(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	testsupport.SeedCompileJob(t, pool, secondCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	stored := key(firstCompileID, "station-a", 30)
	if err := repo.Put(ctx, []store.CachedIsochrone{{Key: stored, Geometry: polygon("station-a")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	differsBy := map[string]store.IsochroneKey{
		"compile job":  key(secondCompileID, "station-a", 30),
		"station slug": key(firstCompileID, "station-b", 30),
		"contour":      key(firstCompileID, "station-a", 31),
		"mode": {
			CompileJobID: firstCompileID, StationSlug: "station-a",
			Mode: "bike", ContourMins: 30,
		},
	}

	for field, k := range differsBy {
		got, err := repo.Get(ctx, []store.IsochroneKey{k})
		if err != nil {
			t.Fatalf("Get differing by %s: %v", field, err)
		}
		if len(got) != 0 {
			t.Errorf("a key differing only by %s was served the stored polygon", field)
		}
	}
}

// The acceptance criterion this repository can actually assert: a recompile
// rotates the generation, and the new one reads none of the old one's entries.
// Nothing is purged — the old rows are still there — they are simply no longer
// addressable.
func TestIsochroneCache_recompileDoesNotReadPreviousGeneration(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	testsupport.SeedCompileJob(t, pool, secondCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	before := key(firstCompileID, "station-a", 30)
	if err := repo.Put(ctx, []store.CachedIsochrone{{Key: before, Geometry: polygon("before")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	after := key(secondCompileID, "station-a", 30)
	got, err := repo.Get(ctx, []store.IsochroneKey{after})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the recompiled graph read %d entries from the previous generation, want 0", len(got))
	}
	if n := testsupport.IsochroneCacheCount(t, pool, firstCompileID); n != 1 {
		t.Errorf("previous generation holds %d rows, want 1 — rotation must not purge anything", n)
	}
}

// A cached polygon that outlived the graph it was plotted over would be a
// polygon nothing can ever address again. The foreign key is what stops that,
// and deleting a scenario or an authored service is what deletes its compile
// job upstream.
func TestIsochroneCache_rowsCascadeWithTheCompileJob(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	if err := repo.Put(ctx, []store.CachedIsochrone{
		{Key: key(firstCompileID, "station-a", 30), Geometry: polygon("a")},
		{Key: key(firstCompileID, "station-b", 20), Geometry: polygon("b")},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n := testsupport.IsochroneCacheCount(t, pool, firstCompileID); n != 2 {
		t.Fatalf("cache rows before delete = %d, want 2", n)
	}

	testsupport.DeleteCompileJob(t, pool, firstCompileID)

	if n := testsupport.IsochroneCacheCount(t, pool, firstCompileID); n != 0 {
		t.Errorf("cache rows survived the compile job by %d, want 0", n)
	}
}

// The tileset stamp is written as data and read by nobody: a key naming a
// different tileset still finds the row, because the key has no such field.
func TestIsochroneCache_tilesetIsRecordedButNotKeyed(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	built := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	k := key(firstCompileID, "station-a", 30)
	if err := repo.Put(ctx, []store.CachedIsochrone{
		{Key: k, Geometry: polygon("a"), TilesetAt: built},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	at := testsupport.IsochroneCacheTilesetAt(t, pool, firstCompileID, "station-a", "walk", 30)
	if at == nil {
		t.Fatal("tileset_at is NULL, want the stamp the entry carried")
	}
	if !at.Equal(built) {
		t.Errorf("tileset_at = %s, want %s", at, built)
	}

	// The lookup key carries no tileset at all, so the row is still found —
	// which is exactly the accepted risk: a rebuild does not invalidate.
	got, err := repo.Get(ctx, []store.IsochroneKey{k})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Get found %d entries, want 1 — the tileset must not narrow the lookup", len(got))
	}
}

// A worker with no tileset to name — the synthetic client has never seen a tile
// — still caches. Storing NULL rather than refusing the row is what keeps the
// local loop identical to the deployed one.
func TestIsochroneCache_zeroTilesetIsStoredAsNull(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	repo := store.NewRepo(pool)

	if err := repo.Put(context.Background(), []store.CachedIsochrone{
		{Key: key(firstCompileID, "station-a", 30), Geometry: polygon("a")},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if at := testsupport.IsochroneCacheTilesetAt(t, pool, firstCompileID, "station-a", "walk", 30); at != nil {
		t.Errorf("tileset_at = %s, want NULL when no tileset was reported", at)
	}
}

// Two workers can chain over the same graph at once, and both will miss and
// both will write. The second write is the same polygon computed from the same
// four inputs, so it is left alone rather than failing the job that made it.
func TestIsochroneCache_repeatedPutIsNotAnError(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedCompileJob(t, pool, firstCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	entry := store.CachedIsochrone{Key: key(firstCompileID, "station-a", 30), Geometry: polygon("first")}
	if err := repo.Put(ctx, []store.CachedIsochrone{entry}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	entry.Geometry = polygon("second")
	if err := repo.Put(ctx, []store.CachedIsochrone{entry}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if n := testsupport.IsochroneCacheCount(t, pool, firstCompileID); n != 1 {
		t.Errorf("cache rows = %d, want 1", n)
	}
}

// Empty batches happen on every chain that finds no reachable station, and on
// every chain that hits on all of them. Neither should reach the database.
func TestIsochroneCache_emptyBatchesAreNoOps(t *testing.T) {
	pool := testsupport.Postgres(t)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	got, err := repo.Get(ctx, nil)
	if err != nil {
		t.Fatalf("Get(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Get(nil) returned %d entries, want 0", len(got))
	}
	if err := repo.Put(ctx, nil); err != nil {
		t.Fatalf("Put(nil): %v", err)
	}
}

// A compile job that cascaded away mid-chain leaves the worker holding a
// polygon with nowhere to put it. That is the foreign key doing its job, and
// the caller is expected to log it rather than fail a routing job whose answer
// is already computed — so the error has to arrive as an error, not a panic.
func TestIsochroneCache_putForUnknownCompileJobErrors(t *testing.T) {
	pool := testsupport.Postgres(t)
	repo := store.NewRepo(pool)

	err := repo.Put(context.Background(), []store.CachedIsochrone{
		{Key: key(firstCompileID, "station-a", 30), Geometry: polygon("a")},
	})
	if err == nil {
		t.Fatal("Put against a compile job that does not exist: want an error, got nil")
	}
}

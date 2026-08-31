-- Test-only schema. NOT a migration, and deliberately not one.
--
-- The real schema lives in sparks-effect-api's migration sequence
-- (00014_routing_jobs.sql, plus 00021's mode CHECK), because routing_jobs and isochrone_cache both want
-- real foreign keys into users and jobs, and a foreign key cannot cross a
-- schema owned by a different migration sequence. This worker only ever updates
-- rows; it runs no migrations, and a second goose sequence against the same
-- database would be two tools racing to own one schema_migrations table.
--
-- What this file is for is giving the integration tests a database to write to
-- without depending on the other repository at build time. It reproduces the
-- columns, types, keys and constraints this worker touches, so a query that
-- works here works there. It is a copy, and copies drift: if a test passes here
-- and the worker fails in a deployed environment, this file disagreeing with
-- 00014_routing_jobs.sql is the first place to look.
--
-- The owning tables are cut down to what the foreign keys need. Keeping the
-- keys real is the point — a cascade that deletes a routing job out from under
-- an in-flight message is a case the worker has to handle, and it cannot be
-- exercised against a table with the references stripped out. The same goes for
-- isochrone_cache: a cached polygon outliving the graph it was plotted over is
-- exactly what the foreign key is there to prevent, so the key is real here too.

-- CASCADE, because the throwaway database is not guaranteed to hold only what
-- this file created: a run against a database an older schema touched — or one
-- the real migration was applied to — leaves tables referencing jobs that are
-- not named here, and a plain DROP would fail on them rather than resetting.
DROP TABLE IF EXISTS isochrone_cache CASCADE;
DROP TABLE IF EXISTS routing_jobs CASCADE;
DROP TABLE IF EXISTS jobs CASCADE;
DROP TABLE IF EXISTS users CASCADE;

CREATE TABLE users (
    id uuid PRIMARY KEY
);

CREATE TABLE jobs (
    id uuid PRIMARY KEY
);

CREATE TABLE routing_jobs (
    id             uuid PRIMARY KEY,
    status         text NOT NULL,
    compile_job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    owner_id       uuid REFERENCES users (id) ON DELETE CASCADE,
    lat            double precision NOT NULL,
    lng            double precision NOT NULL,
    budget_mins    integer NOT NULL,
    -- Same CHECK as sparks-effect-api's 00021. isochrone_cache.mode is
    -- deliberately unconstrained: SPA-248 left it that way because this
    -- worker is the only writer, and a constraint owned by the other
    -- repository would fail this side's inserts if the two copies of the
    -- enum ever drifted. A wrong mode there is a cache miss, not a wrong
    -- isochrone.
    mode           text NOT NULL CHECK (mode IN ('walk', 'bike', 'drive', 'transit')),
    result         jsonb,
    error          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX routing_jobs_status_idx ON routing_jobs (status);

CREATE TABLE isochrone_cache (
    compile_job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    station_slug   text NOT NULL,
    mode           text NOT NULL,
    contour_mins   integer NOT NULL,
    geometry       jsonb NOT NULL,
    -- Diagnostic only, deliberately not part of the key. Nullable, because the
    -- synthetic routing client has no tiles to describe.
    tileset_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (compile_job_id, station_slug, mode, contour_mins)
);

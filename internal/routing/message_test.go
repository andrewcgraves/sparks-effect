package routing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

// testdata/message.golden.json is the same file, byte for byte, that
// sparks-effect-api asserts it produces. It is the only check that the two
// hand-maintained copies of Message still agree, so this test reads the bytes
// off disk rather than round-tripping a struct: a fixture regenerated from this
// repository's own struct would agree with itself no matter how far it had
// drifted from the publisher.
func goldenBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	return b
}

func TestDecodeGoldenFixture(t *testing.T) {
	msg, err := Decode(goldenBytes(t))
	if err != nil {
		t.Fatalf("Decode(golden) = %v, want nil", err)
	}

	if msg.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", msg.SchemaVersion, SchemaVersion)
	}
	if want := "6f9619ff-8b86-d011-b42d-00c04fc964ff"; msg.RoutingJobID != want {
		t.Errorf("RoutingJobID = %q, want %q", msg.RoutingJobID, want)
	}
	if want := "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d"; msg.CompileJobID != want {
		t.Errorf("CompileJobID = %q, want %q", msg.CompileJobID, want)
	}
	if msg.Lat != 37.79 || msg.Lng != -122.397 {
		t.Errorf("origin = (%v, %v), want (37.79, -122.397)", msg.Lat, msg.Lng)
	}
	if msg.BudgetMins != 45 {
		t.Errorf("BudgetMins = %d, want 45", msg.BudgetMins)
	}
	if msg.Mode != transit.TravelModeWalk {
		t.Errorf("Mode = %q, want %q", msg.Mode, transit.TravelModeWalk)
	}
}

// The graph is the largest part of the message and the part with the most room
// to decode into something plausible but wrong — a node list that arrives empty,
// or edges whose endpoints no longer line up with node slugs. Decoding it is
// not enough; it has to still answer the two questions the chainer asks of it.
func TestDecodeGoldenFixtureGraphIsRoutable(t *testing.T) {
	msg, err := Decode(goldenBytes(t))
	if err != nil {
		t.Fatalf("Decode(golden) = %v, want nil", err)
	}

	data := transit.CompiledGraph{Graph: msg.Graph}

	nodes := data.Nodes()
	if len(nodes) != 2 {
		t.Fatalf("Nodes() returned %d nodes, want 2", len(nodes))
	}
	byslug := map[string]transit.Node{}
	for _, n := range nodes {
		byslug[n.Slug] = n
	}
	north, ok := byslug["north"]
	if !ok {
		t.Fatalf("Nodes() has no %q; got %v", "north", byslug)
	}
	if north.Lat != 37.7749 || north.Lng != -122.4194 {
		t.Errorf("north = (%v, %v), want (37.7749, -122.4194)", north.Lat, north.Lng)
	}
	if _, ok := byslug["south"]; !ok {
		t.Fatalf("Nodes() has no %q; got %v", "south", byslug)
	}

	secs, wait, service, ok := data.TravelTimeBetween("north", "south")
	if !ok {
		t.Fatal("TravelTimeBetween(north, south) not ok, want a path")
	}
	if secs != 1800 {
		t.Errorf("vehicle seconds = %d, want 1800", secs)
	}
	if wait != 300 {
		t.Errorf("wait seconds = %d, want 300", wait)
	}
	if service != "svc-express" {
		t.Errorf("service = %q, want %q", service, "svc-express")
	}
}

// The fixture's own field order is the wire order the API emits, and Message's
// field declaration order is what encoding/json would emit from this side. They
// have to match, or a future fixture regenerated here would silently reorder
// the contract.
func TestGoldenFixtureKeyOrderMatchesStruct(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(goldenBytes(t), &raw); err != nil {
		t.Fatalf("unmarshalling golden fixture: %v", err)
	}

	want := []string{
		"schema_version", "routing_job_id", "compile_job_id",
		"graph", "lat", "lng", "budget_mins", "mode",
	}
	if len(raw) != len(want) {
		t.Errorf("fixture has %d keys, Message declares %d: %v", len(raw), len(want), keysOf(raw))
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			t.Errorf("fixture is missing key %q", k)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestDecodeRejectsUnusableMessages(t *testing.T) {
	// Each case mutates one field of the golden fixture, so a case only ever
	// exercises the check it names — the rest of the message is known good.
	tests := []struct {
		name  string
		mutis func(m map[string]any)
		want  error
	}{
		{
			name:  "future schema version",
			mutis: func(m map[string]any) { m["schema_version"] = SchemaVersion + 1 },
			want:  ErrUnsupportedSchema,
		},
		{
			name:  "missing schema version",
			mutis: func(m map[string]any) { delete(m, "schema_version") },
			want:  ErrUnsupportedSchema,
		},
		{
			name:  "no routing job id",
			mutis: func(m map[string]any) { m["routing_job_id"] = "" },
			want:  ErrMalformedMessage,
		},
		{
			name:  "no compile job id",
			mutis: func(m map[string]any) { m["compile_job_id"] = "" },
			want:  ErrMalformedMessage,
		},
		{
			name:  "no graph",
			mutis: func(m map[string]any) { m["graph"] = nil },
			want:  ErrMalformedMessage,
		},
		{
			name:  "unknown mode",
			mutis: func(m map[string]any) { m["mode"] = "teleport" },
			want:  ErrMalformedMessage,
		},
		{
			name:  "zero budget",
			mutis: func(m map[string]any) { m["budget_mins"] = 0 },
			want:  ErrMalformedMessage,
		},
		{
			name:  "negative budget",
			mutis: func(m map[string]any) { m["budget_mins"] = -5 },
			want:  ErrMalformedMessage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(goldenBytes(t), &m); err != nil {
				t.Fatalf("unmarshalling golden fixture: %v", err)
			}
			tc.mutis(m)
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("re-marshalling: %v", err)
			}

			if _, err := Decode(body); !errors.Is(err, tc.want) {
				t.Errorf("Decode() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("not json")); !errors.Is(err, ErrMalformedMessage) {
		t.Errorf("Decode(garbage) error = %v, want %v", err, ErrMalformedMessage)
	}
}

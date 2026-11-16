package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

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
	if want := "3fa85f64-5717-4562-b3fc-2c963f66afa6"; msg.TraceID != want {
		t.Errorf("TraceID = %q, want %q", msg.TraceID, want)
	}
}

func TestDecodeAcceptsMessageWithoutTraceID(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(goldenBytes(t), &m); err != nil {
		t.Fatalf("unmarshalling golden fixture: %v", err)
	}
	delete(m, "trace_id")
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}

	msg, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode(no trace_id) = %v, want nil", err)
	}
	if msg.TraceID != "" {
		t.Errorf("TraceID = %q, want empty", msg.TraceID)
	}
}

func TestDecodeAcceptsTransitMode(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(goldenBytes(t), &m); err != nil {
		t.Fatalf("unmarshalling golden fixture: %v", err)
	}
	m["mode"] = "transit"
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}

	msg, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode(mode=transit) = %v, want nil", err)
	}
	if msg.Mode != transit.TravelModeTransit {
		t.Errorf("Mode = %q, want transit", msg.Mode)
	}
}

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

	south, ok := data.Search(map[string]int{"north": 0})["south"]
	if !ok {
		t.Fatal("Search from north did not reach south, want a path")
	}

	if south.TotalSecs != 2100 {
		t.Errorf("TotalSecs = %d, want 2100", south.TotalSecs)
	}
	if south.BoardWaitSecs != 300 {
		t.Errorf("BoardWaitSecs = %d, want 300", south.BoardWaitSecs)
	}
	if south.HopService != "svc-express" {
		t.Errorf("HopService = %q, want %q", south.HopService, "svc-express")
	}
	if south.PredecessorSlug != "north" {
		t.Errorf("PredecessorSlug = %q, want north", south.PredecessorSlug)
	}

	if south.HopRouteID != "rt-spine" {
		t.Errorf("HopRouteID = %q, want rt-spine", south.HopRouteID)
	}
	if south.HopFromChainageM != 27798.4 || south.HopToChainageM != 8123.9 {
		t.Errorf("hop chainages = (%v, %v), want (27798.4, 8123.9)",
			south.HopFromChainageM, south.HopToChainageM)
	}
}

func TestDecodeMessageWithoutEdgePlacementIsStillRoutable(t *testing.T) {
	older := bytes.NewBuffer(nil)
	var m map[string]any
	if err := json.Unmarshal(goldenBytes(t), &m); err != nil {
		t.Fatalf("unmarshalling golden fixture: %v", err)
	}
	graph := m["graph"].(map[string]any)
	svcs := graph["services"].([]any)
	edge := svcs[0].(map[string]any)["edges"].([]any)[0].(map[string]any)
	delete(edge, "route_id")
	delete(edge, "from_chainage_m")
	delete(edge, "to_chainage_m")
	if err := json.NewEncoder(older).Encode(m); err != nil {
		t.Fatalf("re-encoding: %v", err)
	}

	msg, err := Decode(older.Bytes())
	if err != nil {
		t.Fatalf("Decode(pre-SPA-264 message) = %v, want nil", err)
	}
	south, ok := transit.CompiledGraph{Graph: msg.Graph}.Search(map[string]int{"north": 0})["south"]
	if !ok {
		t.Fatal("Search from north did not reach south; an older message must still route")
	}
	if south.TotalSecs != 2100 {
		t.Errorf("TotalSecs = %d, want 2100 — placement is decoration, not weight", south.TotalSecs)
	}
	if south.HopRouteID != "" {
		t.Errorf("HopRouteID = %q, want empty on a message that carries none", south.HopRouteID)
	}
}

func TestDecodeRejectsUnusableMessages(t *testing.T) {
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

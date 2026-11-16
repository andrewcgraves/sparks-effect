package routing

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestGoldenFixtureHasNoFieldTheStructsDrop(t *testing.T) {
	// Stops at "merge": transit.TransitGraph holds it as json.RawMessage, so a
	// renamed key inside the merge report passes this and the re-encode both.
	dec := json.NewDecoder(bytes.NewReader(goldenBytes(t)))
	dec.DisallowUnknownFields()

	var m Message
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("the fixture carries a field no struct in this repo declares: %v", err)
	}
}

func TestGoldenFixtureSurvivesAReEncode(t *testing.T) {
	raw := goldenBytes(t)

	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshalling the fixture: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}

	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshalling the fixture: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshalling the re-encoded message: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a decode/encode round trip changed the message.\ngot  %s\nwant %s", out, raw)
	}
}

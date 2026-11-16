package valhalla_test

import (
	"strings"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestNewClientRequiresExplicitSynthetic(t *testing.T) {
	_, err := valhalla.NewClient(valhalla.ClientConfig{})
	if err == nil || !strings.Contains(err.Error(), "VALHALLA_URL") {
		t.Fatalf("error = %v, want VALHALLA_URL required", err)
	}
}

func TestNewClientSynthetic(t *testing.T) {
	client, err := valhalla.NewClient(valhalla.ClientConfig{Synthetic: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*valhalla.SyntheticClient); !ok {
		t.Fatalf("got %T, want *SyntheticClient", client)
	}
}

func TestNewClientHTTP(t *testing.T) {
	client, err := valhalla.NewClient(valhalla.ClientConfig{URL: "http://valhalla:8002"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*valhalla.HTTPClient); !ok {
		t.Fatalf("got %T, want *HTTPClient", client)
	}
}

func TestNewClientRejectsURLWithSynthetic(t *testing.T) {
	_, err := valhalla.NewClient(valhalla.ClientConfig{
		URL:       "http://valhalla:8002",
		Synthetic: true,
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want mutually exclusive", err)
	}
}

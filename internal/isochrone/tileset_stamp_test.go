package isochrone_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type statusClient struct {
	valhalla.Client
	status func(context.Context) (valhalla.Status, error)
}

func (c *statusClient) Status(ctx context.Context) (valhalla.Status, error) {
	return c.status(ctx)
}

func TestTilesetStamp_nilReceiverIsSafe(t *testing.T) {
	var stamp *isochrone.TilesetStamp
	stamp.Refresh(context.Background(), nil, nil)
	if !stamp.Time().IsZero() {
		t.Errorf("nil stamp time = %s, want zero", stamp.Time())
	}
}

func TestTilesetStamp_nilLoggerKeepsStampOnStatusError(t *testing.T) {
	known := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	stamp := isochrone.NewTilesetStamp(known)

	stamp.Refresh(context.Background(), &valhalla.StubClient{StatusErr: valhalla.ErrUpstream}, nil)

	if got := stamp.Time(); !got.Equal(known) {
		t.Errorf("stamp = %s, want %s", got, known)
	}
}

func TestTilesetStamp_refresh(t *testing.T) {
	known := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	changed := known.Add(24 * time.Hour)
	statusErr := errors.New("status unavailable")

	tests := []struct {
		name        string
		initial     time.Time
		status      valhalla.Status
		err         error
		want        time.Time
		wantMessage string
	}{
		{
			name:        "first known timestamp",
			status:      valhalla.Status{Version: "3.5.1", TilesetAt: known},
			want:        known,
			wantMessage: "routing: Valhalla tileset",
		},
		{
			name:        "changed timestamp",
			initial:     known,
			status:      valhalla.Status{Version: "3.5.1", TilesetAt: changed},
			want:        changed,
			wantMessage: "routing: Valhalla tileset",
		},
		{
			name:    "unchanged timestamp",
			initial: known,
			status:  valhalla.Status{Version: "3.5.1", TilesetAt: known},
			want:    known,
		},
		{
			name:        "status error keeps known timestamp",
			initial:     known,
			err:         statusErr,
			want:        known,
			wantMessage: "routing: could not read Valhalla /status",
		},
		{
			name:        "missing timestamp keeps known timestamp",
			initial:     known,
			status:      valhalla.Status{Version: "3.5.1"},
			want:        known,
			wantMessage: "routing: Valhalla reports no tileset timestamp",
		},
		{
			name:    "no status keeps known timestamp without logging",
			initial: known,
			err:     errors.Join(valhalla.ErrNoStatus, statusErr),
			want:    known,
		},
		{
			name: "no status keeps zero timestamp without logging",
			err:  errors.Join(valhalla.ErrNoStatus, statusErr),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			stamp := isochrone.NewTilesetStamp(tc.initial)
			client := &valhalla.StubClient{StatusResp: tc.status, StatusErr: tc.err}

			stamp.Refresh(context.Background(), client, logger.New(&out, false))

			if got := stamp.Time(); !got.Equal(tc.want) {
				t.Errorf("stamp = %s, want %s", got, tc.want)
			}
			if tc.wantMessage == "" {
				if out.Len() != 0 {
					t.Errorf("unchanged refresh logged %q", out.String())
				}
			} else if !strings.Contains(out.String(), tc.wantMessage) {
				t.Errorf("log = %q, want message %q", out.String(), tc.wantMessage)
			}
		})
	}
}

func TestTilesetStamp_refreshUsesAShortTimeout(t *testing.T) {
	var timeout time.Duration
	client := &statusClient{
		status: func(ctx context.Context) (valhalla.Status, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("Status context has no deadline")
			}
			timeout = time.Until(deadline)
			return valhalla.Status{}, errors.New("status unavailable")
		},
	}

	isochrone.NewTilesetStamp(time.Time{}).Refresh(context.Background(), client, logger.Discard())

	if timeout < 2*time.Second || timeout > 4*time.Second {
		t.Errorf("Status timeout = %s, want a few seconds", timeout)
	}
}

func TestTilesetStamp_concurrentReadsAndRefreshes(t *testing.T) {
	built := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	stamp := isochrone.NewTilesetStamp(time.Time{})
	client := &valhalla.StubClient{StatusResp: valhalla.Status{TilesetAt: built}}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			stamp.Refresh(context.Background(), client, logger.Discard())
		}()
		go func() {
			defer wg.Done()
			_ = stamp.Time()
		}()
	}
	wg.Wait()

	if got := stamp.Time(); !got.Equal(built) {
		t.Errorf("stamp = %s, want %s", got, built)
	}
}

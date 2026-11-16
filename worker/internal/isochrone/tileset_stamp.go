package isochrone

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

const tilesetStatusTimeout = 3 * time.Second

type TilesetStamp struct {
	mu sync.RWMutex
	at time.Time
}

func NewTilesetStamp(at time.Time) *TilesetStamp {
	return &TilesetStamp{at: at}
}

func (s *TilesetStamp) Time() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.at
}

func (s *TilesetStamp) Refresh(ctx context.Context, client valhalla.Client, log *logger.Logger) {
	if s == nil {
		return
	}

	statusCtx, cancel := context.WithTimeout(ctx, tilesetStatusTimeout)
	defer cancel()

	status, err := client.Status(statusCtx)
	if err != nil {
		if errors.Is(err, valhalla.ErrNoStatus) {
			return
		}
		if log != nil {
			log.Warn(ctx, "routing: could not read Valhalla /status", "err", err.Error())
		}
		return
	}
	if status.TilesetAt.IsZero() {
		if log != nil {
			log.Warn(ctx, "routing: Valhalla reports no tileset timestamp", "valhalla_version", status.Version)
		}
		return
	}

	s.mu.Lock()
	changed := !s.at.Equal(status.TilesetAt)
	s.at = status.TilesetAt
	s.mu.Unlock()

	if changed && log != nil {
		log.Info(ctx, "routing: Valhalla tileset",
			"valhalla_version", status.Version,
			"tileset_at", status.TilesetAt.Format(time.RFC3339))
	}
}

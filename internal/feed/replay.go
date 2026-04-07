package feed

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// ReplayFeed replays a pre-fetched slice of MarketEvents for backtesting.
// It feeds events at wall-clock rate or as fast as possible.
type ReplayFeed struct {
	marketID string
	events   []MarketEvent
	outCh    chan<- MarketEvent
	realtime bool // if true, sleep between events to simulate real time
	log      zerolog.Logger
}

// NewReplayFeed creates a replay feed.
func NewReplayFeed(marketID string, events []MarketEvent, outCh chan<- MarketEvent, realtime bool, log zerolog.Logger) *ReplayFeed {
	return &ReplayFeed{
		marketID: marketID,
		events:   events,
		outCh:    outCh,
		realtime: realtime,
		log:      log.With().Str("market", marketID).Logger(),
	}
}

// Run replays all events and then returns.
func (r *ReplayFeed) Run(ctx context.Context) {
	r.log.Info().Int("events", len(r.events)).Msg("starting replay feed")
	var prev time.Time
	for _, ev := range r.events {
		if ctx.Err() != nil {
			return
		}
		if r.realtime && !prev.IsZero() {
			gap := ev.Timestamp.Sub(prev)
			if gap > 0 {
				select {
				case <-time.After(gap):
				case <-ctx.Done():
					return
				}
			}
		}
		prev = ev.Timestamp
		select {
		case r.outCh <- ev:
		case <-ctx.Done():
			return
		}
	}
	r.log.Info().Msg("replay feed finished")
}

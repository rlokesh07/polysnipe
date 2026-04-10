package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"polysnipe/internal/state"
)

// SpreadThreshold buys the cheaper side when the bid-ask spread exceeds a threshold,
// and closes when the spread contracts below the exit threshold.
type SpreadThreshold struct {
	id   string
	tags []string

	// params
	spreadTriggerBPS int
	spreadExitBPS    int
	side             string // "cheap" (buy whichever is below 0.5) or "yes" or "no"

	// per-market state: current position direction (or Close if none)
	positions map[string]Direction
}

// NewSpreadThreshold creates a new SpreadThreshold strategy.
func NewSpreadThreshold(id string) *SpreadThreshold {
	return &SpreadThreshold{
		id:               id,
		spreadTriggerBPS: 50,
		spreadExitBPS:    20,
		side:             "cheap",
		positions:        make(map[string]Direction),
	}
}

func (s *SpreadThreshold) ID() string      { return s.id }
func (s *SpreadThreshold) Name() string    { return "SpreadThreshold" }
func (s *SpreadThreshold) Tags() []string  { return s.tags }
func (s *SpreadThreshold) SetTags(t []string) { s.tags = t }

func (s *SpreadThreshold) Configure(params map[string]interface{}) error {
	if v, ok := params["spread_trigger_bps"]; ok {
		switch n := v.(type) {
		case int:
			s.spreadTriggerBPS = n
		case float64:
			s.spreadTriggerBPS = int(n)
		default:
			return fmt.Errorf("spread_trigger_bps must be a number")
		}
	}
	if v, ok := params["spread_exit_bps"]; ok {
		switch n := v.(type) {
		case int:
			s.spreadExitBPS = n
		case float64:
			s.spreadExitBPS = int(n)
		default:
			return fmt.Errorf("spread_exit_bps must be a number")
		}
	}
	if v, ok := params["side"]; ok {
		s.side, _ = v.(string)
	}
	return nil
}

func (s *SpreadThreshold) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
	logger := log.With().Str("strategy", s.id).Logger()
	logger.Info().Msg("strategy started")

	for {
		select {
		case <-ctx.Done():
			return

		case update, ok := <-feedbackCh:
			if !ok {
				return
			}
			if update.Status == StatusClosed || update.Status == StatusNone {
				delete(s.positions, update.MarketID)
			}

		case snap, ok := <-snapshotCh:
			if !ok {
				return
			}

			if snap.BestBid.IsZero() || snap.BestAsk.IsZero() {
				continue
			}

			spreadBPS := s.calcSpreadBPS(snap.BestBid, snap.BestAsk, snap.MidPrice)
			held, hasPos := s.positions[snap.MarketID]

			if hasPos {
				// Check exit condition.
				if spreadBPS < int64(s.spreadExitBPS) {
					_ = held
					sig := Signal{
						StrategyID: s.id,
						MarketID:   snap.MarketID,
						Direction:  Close,
						Timestamp:  time.Now(),
					}
					select {
					case signalCh <- sig:
						delete(s.positions, snap.MarketID)
					case <-ctx.Done():
						return
					}
				}
			} else {
				// Check entry condition.
				if spreadBPS >= int64(s.spreadTriggerBPS) {
					dir := s.entryDirection(snap)
					sig := Signal{
						StrategyID: s.id,
						MarketID:   snap.MarketID,
						Direction:  dir,
						Timestamp:  time.Now(),
					}
					select {
					case signalCh <- sig:
						s.positions[snap.MarketID] = dir
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}

// calcSpreadBPS computes spread in basis points relative to mid price.
func (s *SpreadThreshold) calcSpreadBPS(bid, ask, mid decimal.Decimal) int64 {
	if mid.IsZero() {
		return 0
	}
	spread := ask.Sub(bid)
	bps := spread.Mul(decimal.NewFromInt(10000)).Div(mid)
	return bps.IntPart()
}

func (s *SpreadThreshold) entryDirection(snap state.MarketSnapshot) Direction {
	switch s.side {
	case "yes":
		return BuyYes
	case "no":
		return BuyNo
	default: // "cheap"
		// Buy the side trading below 0.5 (the cheaper one).
		half := decimal.NewFromFloat(0.5)
		if snap.BestAsk.LessThan(half) {
			return BuyYes
		}
		return BuyNo
	}
}

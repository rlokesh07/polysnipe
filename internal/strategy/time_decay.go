package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"polysnipe/internal/state"
)

// TimeDecay fades extreme prices near the end of the contract window.
// Within the final N seconds, if YES trades above a skew threshold, bet NO (and vice versa).
type TimeDecay struct {
	id      string
	markets []string

	// params
	triggerRemainingSeconds int
	skewThreshold           float64

	// per-market state
	positions map[string]bool
}

// NewTimeDecay creates a new TimeDecay strategy.
func NewTimeDecay(id string) *TimeDecay {
	return &TimeDecay{
		id:                      id,
		triggerRemainingSeconds: 30,
		skewThreshold:           0.70,
		positions:               make(map[string]bool),
	}
}

func (s *TimeDecay) ID() string        { return s.id }
func (s *TimeDecay) Name() string      { return "TimeDecay" }
func (s *TimeDecay) Markets() []string { return s.markets }
func (s *TimeDecay) SetMarkets(m []string) { s.markets = m }

func (s *TimeDecay) Configure(params map[string]interface{}) error {
	if v, ok := params["trigger_remaining_seconds"]; ok {
		switch n := v.(type) {
		case int:
			s.triggerRemainingSeconds = n
		case float64:
			s.triggerRemainingSeconds = int(n)
		default:
			return fmt.Errorf("trigger_remaining_seconds must be a number")
		}
	}
	if v, ok := params["skew_threshold"]; ok {
		switch n := v.(type) {
		case float64:
			s.skewThreshold = n
		case int:
			s.skewThreshold = float64(n)
		default:
			return fmt.Errorf("skew_threshold must be a number")
		}
	}
	return nil
}

func (s *TimeDecay) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
	logger := log.With().Str("strategy", s.id).Logger()
	logger.Info().Msg("strategy started")

	trigger := time.Duration(s.triggerRemainingSeconds) * time.Second
	threshold := decimal.NewFromFloat(s.skewThreshold)
	antiThreshold := decimal.NewFromFloat(1.0 - s.skewThreshold)

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

			_, hasPos := s.positions[snap.MarketID]
			if hasPos {
				// Hold until window closes — no exit logic here.
				continue
			}

			// Only act in final window.
			if snap.TimeRemaining > trigger || snap.TimeRemaining <= 0 {
				continue
			}

			// Use mid price as proxy for YES probability.
			mid := snap.MidPrice
			if mid.IsZero() && snap.LastPrice.IsPositive() {
				mid = snap.LastPrice
			}
			if mid.IsZero() {
				continue
			}

			var dir Direction
			var triggered bool

			if mid.GreaterThanOrEqual(threshold) {
				// YES is overbought — bet NO.
				dir = BuyNo
				triggered = true
			} else if mid.LessThanOrEqual(antiThreshold) {
				// NO is overbought — bet YES.
				dir = BuyYes
				triggered = true
			}

			if triggered {
				sig := Signal{
					StrategyID: s.id,
					MarketID:   snap.MarketID,
					Direction:  dir,
					Timestamp:  time.Now(),
				}
				select {
				case signalCh <- sig:
					s.positions[snap.MarketID] = true
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

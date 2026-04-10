package strategy

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"polysnipe/internal/state"
)

// Momentum buys after N consecutive price ticks in the same direction,
// each moving at least M basis points. Exits after a hold duration or on reversal.
type Momentum struct {
	id   string
	tags []string

	// params
	consecutiveTicks    int
	minMoveBPS          int
	holdDurationSeconds int

	// per-market state
	marketState map[string]*momentumState
}

type momentumState struct {
	consecutiveUp   int
	consecutiveDown int
	lastPrice       decimal.Decimal
	hasPos          bool
	posDir          Direction
	posOpenedAt     time.Time
}

// NewMomentum creates a new Momentum strategy.
func NewMomentum(id string) *Momentum {
	return &Momentum{
		id:                  id,
		consecutiveTicks:    5,
		minMoveBPS:          10,
		holdDurationSeconds: 60,
		marketState:         make(map[string]*momentumState),
	}
}

func (s *Momentum) ID() string      { return s.id }
func (s *Momentum) Name() string    { return "Momentum" }
func (s *Momentum) Tags() []string  { return s.tags }
func (s *Momentum) SetTags(t []string) { s.tags = t }

func (s *Momentum) Configure(params map[string]interface{}) error {
	if v, ok := params["consecutive_ticks"]; ok {
		switch n := v.(type) {
		case int:
			s.consecutiveTicks = n
		case float64:
			s.consecutiveTicks = int(n)
		default:
			return fmt.Errorf("consecutive_ticks must be a number")
		}
	}
	if v, ok := params["min_move_bps"]; ok {
		switch n := v.(type) {
		case int:
			s.minMoveBPS = n
		case float64:
			s.minMoveBPS = int(n)
		default:
			return fmt.Errorf("min_move_bps must be a number")
		}
	}
	if v, ok := params["hold_duration_seconds"]; ok {
		switch n := v.(type) {
		case int:
			s.holdDurationSeconds = n
		case float64:
			s.holdDurationSeconds = int(n)
		default:
			return fmt.Errorf("hold_duration_seconds must be a number")
		}
	}
	return nil
}

func (s *Momentum) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
	logger := log.With().Str("strategy", s.id).Logger()
	logger.Info().Msg("strategy started")

	holdDuration := time.Duration(s.holdDurationSeconds) * time.Second

	for {
		select {
		case <-ctx.Done():
			return

		case update, ok := <-feedbackCh:
			if !ok {
				return
			}
			if ms, exists := s.marketState[update.MarketID]; exists {
				if update.Status == StatusClosed || update.Status == StatusNone {
					ms.hasPos = false
				}
			}

		case snap, ok := <-snapshotCh:
			if !ok {
				return
			}
			if snap.LastPrice.IsZero() {
				continue
			}

			ms := s.getMarketState(snap.MarketID)

			// Check hold duration expiry.
			if ms.hasPos && time.Since(ms.posOpenedAt) >= holdDuration {
				sig := Signal{
					StrategyID: s.id,
					MarketID:   snap.MarketID,
					Direction:  Close,
					Timestamp:  time.Now(),
				}
				select {
				case signalCh <- sig:
					ms.hasPos = false
				case <-ctx.Done():
					return
				}
				ms.consecutiveUp = 0
				ms.consecutiveDown = 0
				ms.lastPrice = snap.LastPrice
				continue
			}

			if !ms.lastPrice.IsZero() {
				moveBPS := s.calcMoveBPS(ms.lastPrice, snap.LastPrice)
				minBPS := int64(s.minMoveBPS)

				if moveBPS >= minBPS {
					ms.consecutiveUp++
					ms.consecutiveDown = 0
				} else if moveBPS <= -minBPS {
					ms.consecutiveDown++
					ms.consecutiveUp = 0
				} else {
					ms.consecutiveUp = 0
					ms.consecutiveDown = 0
				}

				// Check reversal exit.
				if ms.hasPos {
					if (ms.posDir == BuyYes && ms.consecutiveDown >= s.consecutiveTicks) ||
						(ms.posDir == BuyNo && ms.consecutiveUp >= s.consecutiveTicks) {
						sig := Signal{
							StrategyID: s.id,
							MarketID:   snap.MarketID,
							Direction:  Close,
							Timestamp:  time.Now(),
						}
						select {
						case signalCh <- sig:
							ms.hasPos = false
						case <-ctx.Done():
							return
						}
					}
				}

				// Check entry.
				if !ms.hasPos {
					var dir Direction
					var triggered bool
					if ms.consecutiveUp >= s.consecutiveTicks {
						dir = BuyYes
						triggered = true
					} else if ms.consecutiveDown >= s.consecutiveTicks {
						dir = BuyNo
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
							ms.hasPos = true
							ms.posDir = dir
							ms.posOpenedAt = time.Now()
							ms.consecutiveUp = 0
							ms.consecutiveDown = 0
						case <-ctx.Done():
							return
						}
					}
				}
			}

			ms.lastPrice = snap.LastPrice
		}
	}
}

func (s *Momentum) getMarketState(marketID string) *momentumState {
	if ms, ok := s.marketState[marketID]; ok {
		return ms
	}
	ms := &momentumState{}
	s.marketState[marketID] = ms
	return ms
}

// calcMoveBPS returns the move in basis points (positive = up, negative = down).
func (s *Momentum) calcMoveBPS(prev, curr decimal.Decimal) int64 {
	if prev.IsZero() {
		return 0
	}
	move := curr.Sub(prev).Mul(decimal.NewFromInt(10000)).Div(prev)
	return move.IntPart()
}

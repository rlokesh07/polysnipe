package strategy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"polysnipe/internal/state"
)

// SMAReversion buys the direction expected to revert to the mean when the mid price
// diverges from its Simple Moving Average. Only trades markets between minPrice and maxPrice.
type SMAReversion struct {
	id   string
	tags []string

	// params
	smaPeriod    int     // rolling window size (default: 20)
	minPrice     float64 // lower bound to trade (default: 0.20)
	maxPrice     float64 // upper bound to trade (default: 0.80)
	stopLossCents float64 // close if YES mid moves this many cents against position (default: 2.0)

	// per-market state — guarded by mu because Run is called per-market in separate goroutines
	mu     sync.Mutex
	states map[string]*smaMarketState
}

type smaMarketState struct {
	prices       []decimal.Decimal // rolling price window, len <= smaPeriod
	hasPos       bool              // true only after entry order is confirmed filled
	pendingEntry bool              // true after signal sent, before fill/cancel feedback
	posDir       Direction
	entryYesMid  decimal.Decimal // YES mid at entry, used for stop-loss
	aboveSMA     *bool           // which side of SMA price was on last tick (nil = unknown)
}

// NewSMAReversion creates a new SMAReversion strategy.
func NewSMAReversion(id string) *SMAReversion {
	return &SMAReversion{
		id:            id,
		smaPeriod:     20,
		minPrice:      0.20,
		maxPrice:      0.80,
		stopLossCents: 2.0,
		states:        make(map[string]*smaMarketState),
	}
}

func (s *SMAReversion) ID() string         { return s.id }
func (s *SMAReversion) Name() string       { return "SMAReversion" }
func (s *SMAReversion) Tags() []string     { return s.tags }
func (s *SMAReversion) SetTags(t []string) { s.tags = t }

func (s *SMAReversion) Configure(params map[string]interface{}) error {
	if v, ok := params["sma_period"]; ok {
		switch n := v.(type) {
		case int:
			s.smaPeriod = n
		case float64:
			s.smaPeriod = int(n)
		default:
			return fmt.Errorf("sma_period must be a number")
		}
		if s.smaPeriod < 2 {
			return fmt.Errorf("sma_period must be >= 2")
		}
	}
	if v, ok := params["min_price"]; ok {
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("min_price must be a float")
		}
		s.minPrice = f
	}
	if v, ok := params["max_price"]; ok {
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("max_price must be a float")
		}
		s.maxPrice = f
	}
	if v, ok := params["stop_loss_cents"]; ok {
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("stop_loss_cents must be a float")
		}
		s.stopLossCents = f
	}
	return nil
}

func (s *SMAReversion) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
	logger := log.With().Str("strategy", s.id).Logger()
	logger.Info().Msg("strategy started")

	minDec := decimal.NewFromFloat(s.minPrice)
	maxDec := decimal.NewFromFloat(s.maxPrice)
	one := decimal.NewFromFloat(1.0)
	stopLoss := decimal.NewFromFloat(s.stopLossCents / 100.0)

	for {
		select {
		case <-ctx.Done():
			return

		case update, ok := <-feedbackCh:
			if !ok {
				return
			}
			if ms, exists := s.states[update.MarketID]; exists {
				switch update.Status {
				case StatusOpen:
					// Entry order confirmed filled — position is now live.
					ms.hasPos = true
					ms.pendingEntry = false
				case StatusClosed, StatusNone:
					ms.hasPos = false
					ms.pendingEntry = false
					ms.prices = ms.prices[:0]
					ms.aboveSMA = nil
				}
			}

		case snap, ok := <-snapshotCh:
			if !ok {
				return
			}

			// Use MidPrice; fall back to LastPrice.
			mid := snap.MidPrice
			if mid.IsZero() {
				mid = snap.LastPrice
			}
			if mid.IsZero() {
				continue
			}

			ms := s.getState(snap.MarketID)

			// Detect market resolution: close any position when price approaches a terminal value.
			// mid ≤ 0.03 → resolving NO  (bad for BuyYes, good for BuyNo but trade is done)
			// mid ≥ 0.97 → resolving YES (good for BuyYes, bad for BuyNo)
			if ms.hasPos {
				nearZero := decimal.NewFromFloat(0.03)
				nearOne := decimal.NewFromFloat(0.97)
				resolving := mid.LessThanOrEqual(nearZero) || mid.GreaterThanOrEqual(nearOne)
				if resolving {
					sig := Signal{
						StrategyID: s.id,
						MarketID:   snap.MarketID,
						Direction:  Close,
						Price:      mid,
						Timestamp:  time.Now(),
					}
					select {
					case signalCh <- sig:
						logger.Warn().
							Str("market_id", snap.MarketID).
							Str("mid", mid.String()).
							Str("side", ms.posDir.String()).
							Msg("market resolving; closing position")
						ms.hasPos = false
						ms.prices = ms.prices[:0]
						ms.aboveSMA = nil
					case <-ctx.Done():
						return
					}
					continue
				}
			}

			// Append to rolling window and cap at smaPeriod.
			ms.prices = append(ms.prices, mid)
			if len(ms.prices) > s.smaPeriod {
				ms.prices = ms.prices[len(ms.prices)-s.smaPeriod:]
			}

			// Wait until window is full.
			if len(ms.prices) < s.smaPeriod {
				continue
			}

			sma := s.computeSMA(ms.prices)

			// While an entry order is pending (submitted but not yet filled/rejected),
			// skip all entry and exit logic. The position isn't live yet.
			if ms.pendingEntry {
				continue
			}

			// Exit logic.
			if ms.hasPos {
				// Stop-loss: close if YES mid moves more than stopLoss against the position.
				hitStop := false
				if ms.posDir == BuyYes && ms.entryYesMid.Sub(mid).GreaterThan(stopLoss) {
					hitStop = true
				} else if ms.posDir == BuyNo && mid.Sub(ms.entryYesMid).GreaterThan(stopLoss) {
					hitStop = true
				}

				// SMA cross-back: trend reversed through the mean — exit.
				crossedSMA := (ms.posDir == BuyYes && mid.LessThanOrEqual(sma)) ||
					(ms.posDir == BuyNo && mid.GreaterThanOrEqual(sma))

				if hitStop || crossedSMA {
					reason := "sma_crossback"
					if hitStop {
						reason = "stop_loss"
					}
					sig := Signal{
						StrategyID: s.id,
						MarketID:   snap.MarketID,
						Direction:  Close,
						Price:      mid,
						Timestamp:  time.Now(),
					}
					select {
					case signalCh <- sig:
						logger.Info().
							Str("market_id", snap.MarketID).
							Str("reason", reason).
							Str("side", ms.posDir.String()).
							Str("entry", ms.entryYesMid.String()).
							Str("mid", mid.String()).
							Msg("closing position")
						ms.hasPos = false
						ms.prices = ms.prices[:0]
						ms.aboveSMA = nil
					case <-ctx.Done():
						return
					}
				}
				continue
			}

			// Track which side of the SMA price is on this tick.
			currentlyAbove := mid.GreaterThan(sma)
			prevAbove := ms.aboveSMA
			ms.aboveSMA = &currentlyAbove

			// Only enter on a fresh crossover — the tick price moves from one side to the other.
			// If prevAbove is nil the window just warmed up; skip until we see an actual cross.
			if prevAbove == nil || *prevAbove == currentlyAbove {
				continue
			}

			// Entry logic: only trade within the price band.
			if mid.LessThan(minDec) || mid.GreaterThan(maxDec) {
				continue
			}

			var dir Direction
			var contractPrice decimal.Decimal
			if currentlyAbove {
				// Price just crossed above SMA → trending up → buy YES.
				dir = BuyYes
				contractPrice = mid
			} else {
				// Price just crossed below SMA → trending down → buy NO.
				dir = BuyNo
				contractPrice = one.Sub(mid)
			}

			sig := Signal{
				StrategyID: s.id,
				MarketID:   snap.MarketID,
				Direction:  dir,
				Price:      contractPrice,
				Timestamp:  time.Now(),
			}
			select {
			case signalCh <- sig:
				// Mark pending — hasPos stays false until executor confirms fill.
				ms.pendingEntry = true
				ms.posDir = dir
				ms.entryYesMid = mid
			case <-ctx.Done():
				return
			}
		}
	}
}

func (s *SMAReversion) getState(marketID string) *smaMarketState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms, ok := s.states[marketID]; ok {
		return ms
	}
	ms := &smaMarketState{}
	s.states[marketID] = ms
	return ms
}

func (s *SMAReversion) computeSMA(prices []decimal.Decimal) decimal.Decimal {
	sum := decimal.Zero
	for _, p := range prices {
		sum = sum.Add(p)
	}
	return sum.Div(decimal.NewFromInt(int64(len(prices))))
}

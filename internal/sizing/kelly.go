package sizing

import (
	"sync"

	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
)

// tradeHistory tracks win/loss statistics for a single strategy.
type tradeHistory struct {
	wins      int
	losses    int
	totalWin  decimal.Decimal
	totalLoss decimal.Decimal
}

// KellySizer sizes positions using the Kelly Criterion.
// Falls back to a fixed fraction if sample size is insufficient.
type KellySizer struct {
	cfg     config.SizingConfig
	mu      sync.Mutex
	history map[string]*tradeHistory
	fixed   *FixedSizer
}

// NewKellySizer creates a Kelly criterion sizer.
func NewKellySizer(cfg config.SizingConfig) *KellySizer {
	return &KellySizer{
		cfg:     cfg,
		history: make(map[string]*tradeHistory),
		fixed:   NewFixedSizer(cfg),
	}
}

// Size returns the position size for a given balance and strategy.
func (k *KellySizer) Size(balance decimal.Decimal, strategyID string) decimal.Decimal {
	k.mu.Lock()
	h := k.getHistory(strategyID)
	total := h.wins + h.losses
	wins := h.wins
	var totalWin, totalLoss decimal.Decimal
	totalWin = h.totalWin
	totalLoss = h.totalLoss
	k.mu.Unlock()

	if total < k.cfg.KellyMinSampleSize {
		return k.fixed.Size(balance, strategyID)
	}

	// Kelly formula: f* = (p * b - q) / b
	// where p = win rate, q = 1-p, b = avg win / avg loss ratio
	p := decimal.NewFromFloat(float64(wins) / float64(total))
	q := decimal.NewFromInt(1).Sub(p)

	if wins == 0 || totalWin.IsZero() {
		return k.fixed.Size(balance, strategyID)
	}

	avgWin := totalWin.Div(decimal.NewFromInt(int64(wins)))
	losses := total - wins
	var avgLoss decimal.Decimal
	if losses > 0 && totalLoss.IsPositive() {
		avgLoss = totalLoss.Div(decimal.NewFromInt(int64(losses)))
	}
	if avgLoss.IsZero() {
		avgLoss = decimal.NewFromFloat(1.0)
	}

	b := avgWin.Div(avgLoss)
	// f* = (p*b - q) / b
	kellyFrac := p.Mul(b).Sub(q).Div(b)

	if kellyFrac.IsNegative() || kellyFrac.IsZero() {
		// Negative Kelly means don't bet — return minimum.
		return decimal.NewFromFloat(k.cfg.MinOrderSize)
	}

	// Apply fraction multiplier (e.g., quarter-Kelly).
	fraction := decimal.NewFromFloat(k.cfg.KellyFraction)
	size := balance.Mul(kellyFrac).Mul(fraction)

	return k.clamp(size)
}

// RecordTrade updates the historical win/loss statistics.
func (k *KellySizer) RecordTrade(strategyID string, won bool, pnl decimal.Decimal) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.getHistory(strategyID)
	if won {
		h.wins++
		h.totalWin = h.totalWin.Add(pnl.Abs())
	} else {
		h.losses++
		h.totalLoss = h.totalLoss.Add(pnl.Abs())
	}
}

func (k *KellySizer) getHistory(strategyID string) *tradeHistory {
	if h, ok := k.history[strategyID]; ok {
		return h
	}
	h := &tradeHistory{}
	k.history[strategyID] = h
	return h
}

func (k *KellySizer) clamp(size decimal.Decimal) decimal.Decimal {
	min := decimal.NewFromFloat(k.cfg.MinOrderSize)
	max := decimal.NewFromFloat(k.cfg.MaxOrderSize)
	if size.LessThan(min) {
		return min
	}
	if size.GreaterThan(max) {
		return max
	}
	return size
}

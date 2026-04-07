package sizing

import (
	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
)

// Sizer determines position size for a given balance and strategy.
type Sizer interface {
	Size(balance decimal.Decimal, strategyID string) decimal.Decimal
	RecordTrade(strategyID string, won bool, pnl decimal.Decimal)
}

// FixedSizer sizes positions as a fixed fraction of available balance.
type FixedSizer struct {
	cfg config.SizingConfig
}

// NewFixedSizer creates a fixed-fraction sizer.
func NewFixedSizer(cfg config.SizingConfig) *FixedSizer {
	return &FixedSizer{cfg: cfg}
}

// Size returns a fixed fraction of balance, clamped to min/max order size.
func (f *FixedSizer) Size(balance decimal.Decimal, _ string) decimal.Decimal {
	fraction := decimal.NewFromFloat(f.cfg.FixedFractionFallback)
	size := balance.Mul(fraction)

	min := decimal.NewFromFloat(f.cfg.MinOrderSize)
	max := decimal.NewFromFloat(f.cfg.MaxOrderSize)

	if size.LessThan(min) {
		return min
	}
	if size.GreaterThan(max) {
		return max
	}
	return size
}

// RecordTrade is a no-op for the fixed sizer.
func (f *FixedSizer) RecordTrade(_ string, _ bool, _ decimal.Decimal) {}

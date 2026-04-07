package state

import (
	"time"

	"github.com/shopspring/decimal"

	"polysnipe/internal/feed"
)

// MarketSnapshot is an immutable snapshot of a market's state at a point in time.
// Strategies receive value copies — never pointers.
type MarketSnapshot struct {
	MarketID      string
	Timestamp     time.Time
	BestBid       decimal.Decimal
	BestAsk       decimal.Decimal
	Spread        decimal.Decimal
	MidPrice      decimal.Decimal
	LastPrice     decimal.Decimal
	Volume24h     decimal.Decimal
	TimeRemaining time.Duration
	OrderBook     feed.OrderBookSnapshot
}

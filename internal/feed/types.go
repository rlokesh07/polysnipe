package feed

import (
	"time"

	"github.com/shopspring/decimal"
)

// OrderBookLevel is a single price level in the order book.
type OrderBookLevel struct {
	Price decimal.Decimal
	Size  decimal.Decimal
}

// OrderBookSnapshot is an immutable snapshot of the order book.
type OrderBookSnapshot struct {
	Bids []OrderBookLevel
	Asks []OrderBookLevel
}

// BestBid returns the highest bid price, or zero if empty.
func (o OrderBookSnapshot) BestBid() decimal.Decimal {
	if len(o.Bids) == 0 {
		return decimal.Zero
	}
	best := o.Bids[0].Price
	for _, b := range o.Bids[1:] {
		if b.Price.GreaterThan(best) {
			best = b.Price
		}
	}
	return best
}

// BestAsk returns the lowest ask price, or zero if empty.
func (o OrderBookSnapshot) BestAsk() decimal.Decimal {
	if len(o.Asks) == 0 {
		return decimal.Zero
	}
	best := o.Asks[0].Price
	for _, a := range o.Asks[1:] {
		if a.Price.LessThan(best) {
			best = a.Price
		}
	}
	return best
}

// MarketEvent is a normalized event from the Polymarket feed.
type MarketEvent struct {
	MarketID  string
	Timestamp time.Time
	EventType string // "trade", "book_update", "tick"
	BestBid   decimal.Decimal
	BestAsk   decimal.Decimal
	LastPrice decimal.Decimal
	Volume    decimal.Decimal
	OrderBook OrderBookSnapshot
}

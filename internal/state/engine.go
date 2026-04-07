package state

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/feed"
)

// Engine consumes MarketEvents for a single market, maintains rolling state,
// produces MarketSnapshot structs, and fans them out to subscriber channels.
type Engine struct {
	marketID      string
	windowEnd     time.Time // end of the current contract window
	log           zerolog.Logger

	mu          sync.Mutex
	current     MarketSnapshot
	subscribers []chan MarketSnapshot
}

// NewEngine creates a new market state engine.
// windowEnd is the time when the current contract window closes.
func NewEngine(marketID string, windowEnd time.Time, log zerolog.Logger) *Engine {
	return &Engine{
		marketID:  marketID,
		windowEnd: windowEnd,
		log:       log.With().Str("market", marketID).Logger(),
	}
}

// Subscribe registers a strategy's input channel. Must be called before Run.
// The channel must have capacity 1 (enforced by convention).
func (e *Engine) Subscribe(ch chan MarketSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.subscribers = append(e.subscribers, ch)
}

// Run starts the state engine goroutine. It blocks until ctx is cancelled or inCh is closed.
func (e *Engine) Run(ctx context.Context, inCh <-chan feed.MarketEvent) {
	e.log.Info().Msg("state engine started")
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-inCh:
			if !ok {
				return
			}
			snap := e.update(ev)
			e.fanOut(snap)
		}
	}
}

// Snapshot returns the current market snapshot (thread-safe).
func (e *Engine) Snapshot() MarketSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

func (e *Engine) update(ev feed.MarketEvent) MarketSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	prev := e.current

	snap := MarketSnapshot{
		MarketID:  e.marketID,
		Timestamp: ev.Timestamp,
		OrderBook: ev.OrderBook,
	}

	// Update best bid/ask from event or book.
	if ev.BestBid.IsPositive() {
		snap.BestBid = ev.BestBid
	} else {
		snap.BestBid = prev.BestBid
	}
	if ev.BestAsk.IsPositive() {
		snap.BestAsk = ev.BestAsk
	} else {
		snap.BestAsk = prev.BestAsk
	}

	// If we got a book update, re-derive best bid/ask from the book.
	if ev.EventType == "book_update" {
		if bb := ev.OrderBook.BestBid(); bb.IsPositive() {
			snap.BestBid = bb
		}
		if ba := ev.OrderBook.BestAsk(); ba.IsPositive() {
			snap.BestAsk = ba
		}
	}

	// Derived fields.
	if snap.BestAsk.IsPositive() && snap.BestBid.IsPositive() {
		snap.Spread = snap.BestAsk.Sub(snap.BestBid)
		snap.MidPrice = snap.BestBid.Add(snap.BestAsk).Div(decimal.NewFromInt(2))
	} else {
		snap.Spread = prev.Spread
		snap.MidPrice = prev.MidPrice
	}

	// Last price.
	if ev.LastPrice.IsPositive() {
		snap.LastPrice = ev.LastPrice
	} else {
		snap.LastPrice = prev.LastPrice
	}

	// Volume (accumulate within session — reset is handled externally).
	snap.Volume24h = prev.Volume24h.Add(ev.Volume)

	// Time remaining in contract window.
	snap.TimeRemaining = time.Until(e.windowEnd)
	if snap.TimeRemaining < 0 {
		snap.TimeRemaining = 0
	}

	e.current = snap
	return snap
}

// fanOut sends a snapshot to all subscribers using a drop-oldest pattern.
// If a subscriber's channel is full, drain it and replace with the new snapshot.
func (e *Engine) fanOut(snap MarketSnapshot) {
	e.mu.Lock()
	subs := make([]chan MarketSnapshot, len(e.subscribers))
	copy(subs, e.subscribers)
	e.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
			// Channel full — drain old and send new.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snap:
			default:
			}
		}
	}
}

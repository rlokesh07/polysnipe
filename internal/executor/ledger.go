package executor

import (
	"fmt"
	"sync"

	"github.com/shopspring/decimal"

	"polysnipe/internal/strategy"
)

// Ledger tracks positions and orders keyed by (strategyID, marketID).
// It enforces single-position-per-strategy-per-market semantics.
type Ledger struct {
	mu        sync.Mutex
	positions map[positionKey]*Position
	orders    map[string]*Order // orderID -> order
}

// NewLedger creates an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{
		positions: make(map[positionKey]*Position),
		orders:    make(map[string]*Order),
	}
}

// GetPosition returns the current position for a (strategy, market) pair.
// Returns nil if no position exists.
func (l *Ledger) GetPosition(strategyID, marketID string) *Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.positions[positionKey{strategyID, marketID}]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// HasOpenPosition returns true if the strategy has an open position in this market.
func (l *Ledger) HasOpenPosition(strategyID, marketID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.positions[positionKey{strategyID, marketID}]
	return p != nil && p.Status == strategy.StatusOpen
}

// OpenPosition records a new open position. Returns error if one already exists.
func (l *Ledger) OpenPosition(pos Position) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := positionKey{pos.StrategyID, pos.MarketID}
	if existing := l.positions[k]; existing != nil && existing.Status == strategy.StatusOpen {
		return fmt.Errorf("strategy %s already has open position in market %s", pos.StrategyID, pos.MarketID)
	}
	pos.Status = strategy.StatusOpen
	l.positions[k] = &pos
	return nil
}

// ClosePosition marks the position as closed.
func (l *Ledger) ClosePosition(strategyID, marketID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := positionKey{strategyID, marketID}
	p := l.positions[k]
	if p == nil {
		return fmt.Errorf("no position found for strategy %s market %s", strategyID, marketID)
	}
	p.Status = strategy.StatusClosed
	p.OpenOrderID = ""
	return nil
}

// AddOrder records an order in the ledger.
func (l *Ledger) AddOrder(o Order) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.orders[o.ID] = &o
}

// UpdateOrder updates an existing order.
func (l *Ledger) UpdateOrder(o Order) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.orders[o.ID]; ok {
		*existing = o
	}
}

// GetOrder returns a copy of the order by ID.
func (l *Ledger) GetOrder(orderID string) (*Order, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.orders[orderID]
	if !ok {
		return nil, false
	}
	cp := *o
	return &cp, true
}

// AllPositions returns a snapshot of all positions.
func (l *Ledger) AllPositions() []Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Position, 0, len(l.positions))
	for _, p := range l.positions {
		out = append(out, *p)
	}
	return out
}

// OpenPositions returns all positions with StatusOpen.
func (l *Ledger) OpenPositions() []Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Position
	for _, p := range l.positions {
		if p.Status == strategy.StatusOpen {
			out = append(out, *p)
		}
	}
	return out
}

// UpdatePositionSize adjusts the size of an open position (used for partial fills).
func (l *Ledger) UpdatePositionSize(strategyID, marketID string, size decimal.Decimal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := positionKey{strategyID, marketID}
	if p, ok := l.positions[k]; ok {
		p.Size = size
	}
}

// ValidateClose checks that a Close signal is valid — the strategy must own the position.
func (l *Ledger) ValidateClose(strategyID, marketID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.positions[positionKey{strategyID, marketID}]
	if p == nil || p.Status != strategy.StatusOpen {
		return fmt.Errorf("strategy %s has no open position in market %s to close", strategyID, marketID)
	}
	return nil
}

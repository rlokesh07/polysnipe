package executor

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"polysnipe/internal/strategy"
)

// OrderState mirrors strategy.OrderState for internal ledger use.
type OrderState int

const (
	OrderPending     OrderState = iota
	OrderPartialFill
	OrderFilled
	OrderCancelled
	OrderExpired
)

func (o OrderState) ToStrategy() strategy.OrderState {
	return strategy.OrderState(o)
}

// Order represents a live or simulated limit order.
type Order struct {
	ID          string
	StrategyID  string
	MarketID    string
	Side        strategy.Direction
	IsClose     bool // true for exit/close orders, false for entry orders
	Price       decimal.Decimal
	Size        decimal.Decimal
	FilledSize  decimal.Decimal
	State       OrderState
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Position tracks an open position for a (strategy, market) pair.
type Position struct {
	StrategyID  string
	MarketID    string
	Side        strategy.Direction
	EntryPrice  decimal.Decimal
	Size        decimal.Decimal
	EntryFee    decimal.Decimal // fee paid at entry, used for accurate PnL reporting
	OpenOrderID string
	Status      strategy.PositionStatus
}

// positionKey is the composite key for the position ledger.
type positionKey struct {
	strategyID string
	marketID   string
}

// Executor is the interface for both live and simulated executors.
type Executor interface {
	// Run starts the executor goroutine. feedbackChs maps strategyID to its feedback channel.
	// Bidirectional channels are required so the executor can drain stale updates (drop-oldest).
	Run(ctx context.Context, signalCh <-chan strategy.Signal, feedbackChs map[string]chan strategy.PositionUpdate)

	// CancelAll cancels all open/pending orders.
	CancelAll(ctx context.Context) error

	// CloseAll places close orders for all open positions and waits for fills (with timeout).
	CloseAll(ctx context.Context) error

	// CloseMarket closes all open positions in the given market.
	CloseMarket(ctx context.Context, marketID string) error

	// Positions returns a snapshot of all current positions.
	Positions() []Position
}

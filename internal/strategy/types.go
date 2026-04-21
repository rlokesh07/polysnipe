package strategy

import (
	"time"

	"github.com/shopspring/decimal"
)

// Direction represents the direction of a trade signal.
type Direction int

const (
	BuyYes  Direction = iota
	BuyNo
	Close
	SellYes // exit a BuyYes position: SELL YES tokens back for USDC
	SellNo  // exit a BuyNo position: SELL NO tokens back for USDC
)

func (d Direction) String() string {
	switch d {
	case BuyYes:
		return "BuyYes"
	case BuyNo:
		return "BuyNo"
	case Close:
		return "Close"
	case SellYes:
		return "SellYes"
	case SellNo:
		return "SellNo"
	default:
		return "Unknown"
	}
}

// Signal is emitted by a strategy to request an order from the executor.
type Signal struct {
	StrategyID string
	MarketID   string
	Direction  Direction
	Timestamp  time.Time
	Price      decimal.Decimal // optional: suggested fill price (used by simulated executor)
}

// PositionStatus represents the current state of a strategy's position.
type PositionStatus int

const (
	StatusNone   PositionStatus = iota
	StatusOpen
	StatusClosed
)

// OrderState represents the lifecycle state of a limit order.
type OrderState int

const (
	OrderPending     OrderState = iota
	OrderPartialFill
	OrderFilled
	OrderCancelled
	OrderExpired
)

// PositionUpdate is sent by the executor back to the originating strategy
// to report the current state of its position.
type PositionUpdate struct {
	StrategyID  string
	MarketID    string
	Status      PositionStatus
	Side        Direction
	EntryPrice  decimal.Decimal
	Size        decimal.Decimal
	OpenOrderID string // empty if no pending order
	OrderState  OrderState
}

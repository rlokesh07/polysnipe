package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
	"polysnipe/internal/risk"
	"polysnipe/internal/sizing"
	"polysnipe/internal/strategy"
)

// Fill represents a simulated order fill, recorded for the backtest report.
type Fill struct {
	OrderID    string
	StrategyID string
	MarketID   string
	Side       strategy.Direction
	Price      decimal.Decimal
	Size       decimal.Decimal
	Fee        decimal.Decimal
	Timestamp  time.Time
}

// RejectedSignal records a signal that was rejected and why.
type RejectedSignal struct {
	Signal strategy.Signal
	Reason string
}

// SimulatedExecutor executes orders in simulation mode for backtesting.
type SimulatedExecutor struct {
	cfg        config.ExecutionConfig
	sizingCfg  config.SizingConfig
	risk       *risk.Manager
	sizer      sizing.Sizer
	ledger     *Ledger
	log        zerolog.Logger
	feeRateBPS int
	fillModel  string // "optimistic" or "midpoint"

	mu          sync.Mutex
	balance     decimal.Decimal
	feedbackChs map[string]chan strategy.PositionUpdate

	// backtest outputs
	Fills           []Fill
	RejectedSignals []RejectedSignal

	nextOrderID int64
}

// NewSimulatedExecutor creates a backtest executor.
func NewSimulatedExecutor(
	cfg config.ExecutionConfig,
	sizingCfg config.SizingConfig,
	riskMgr *risk.Manager,
	sizer sizing.Sizer,
	balance decimal.Decimal,
	feeRateBPS int,
	fillModel string,
	log zerolog.Logger,
) *SimulatedExecutor {
	return &SimulatedExecutor{
		cfg:        cfg,
		sizingCfg:  sizingCfg,
		risk:       riskMgr,
		sizer:      sizer,
		ledger:     NewLedger(),
		log:        log.With().Str("component", "simulated_executor").Logger(),
		feeRateBPS: feeRateBPS,
		fillModel:  fillModel,
		balance:    balance,
	}
}

// Run starts the simulated executor goroutine.
func (e *SimulatedExecutor) Run(ctx context.Context, signalCh <-chan strategy.Signal, feedbackChs map[string]chan strategy.PositionUpdate) {
	e.mu.Lock()
	e.feedbackChs = feedbackChs
	e.mu.Unlock()

	e.log.Info().Msg("simulated executor started")

	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-signalCh:
			if !ok {
				return
			}
			e.handleSignal(sig)
		}
	}
}

func (e *SimulatedExecutor) handleSignal(sig strategy.Signal) {
	e.mu.Lock()
	balance := e.balance
	e.mu.Unlock()

	if sig.Direction == strategy.Close {
		if err := e.ledger.ValidateClose(sig.StrategyID, sig.MarketID); err != nil {
			e.recordRejection(sig, err.Error())
			return
		}
		e.simulateClose(sig)
		return
	}

	if e.ledger.HasOpenPosition(sig.StrategyID, sig.MarketID) {
		e.recordRejection(sig, "already has open position")
		return
	}

	size := e.sizer.Size(balance, sig.StrategyID)
	if err := e.risk.Check(sig, sig.StrategyID, size); err != nil {
		e.recordRejection(sig, err.Error())
		return
	}

	e.simulateEntry(sig, size)
}

func (e *SimulatedExecutor) simulateEntry(sig strategy.Signal, size decimal.Decimal) {
	// Simulate fill price based on fill model.
	fillPrice := e.simulateFillPrice(sig)

	orderID := e.newOrderID()
	fee := size.Mul(decimal.NewFromFloat(float64(e.feeRateBPS) / 10000.0))

	e.mu.Lock()
	e.balance = e.balance.Sub(size).Sub(fee)
	e.mu.Unlock()

	pos := Position{
		StrategyID:  sig.StrategyID,
		MarketID:    sig.MarketID,
		Side:        sig.Direction,
		EntryPrice:  fillPrice,
		Size:        size,
		OpenOrderID: orderID,
		Status:      strategy.StatusOpen,
	}
	if err := e.ledger.OpenPosition(pos); err != nil {
		e.log.Error().Err(err).Msg("ledger open position failed in simulation")
		return
	}

	e.risk.RecordOpen(sig.StrategyID, sig.MarketID, size)

	fill := Fill{
		OrderID:    orderID,
		StrategyID: sig.StrategyID,
		MarketID:   sig.MarketID,
		Side:       sig.Direction,
		Price:      fillPrice,
		Size:       size,
		Fee:        fee,
		Timestamp:  sig.Timestamp,
	}
	e.mu.Lock()
	e.Fills = append(e.Fills, fill)
	e.mu.Unlock()

	e.log.Debug().
		Str("strategy", sig.StrategyID).
		Str("market", sig.MarketID).
		Str("side", sig.Direction.String()).
		Str("price", fillPrice.String()).
		Str("size", size.String()).
		Msg("simulated fill")

	e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
		StrategyID:  sig.StrategyID,
		MarketID:    sig.MarketID,
		Status:      strategy.StatusOpen,
		Side:        sig.Direction,
		EntryPrice:  fillPrice,
		Size:        size,
		OpenOrderID: orderID,
		OrderState:  strategy.OrderFilled,
	})
}

func (e *SimulatedExecutor) simulateClose(sig strategy.Signal) {
	pos := e.ledger.GetPosition(sig.StrategyID, sig.MarketID)
	if pos == nil {
		return
	}

	closePrice := e.simulateFillPrice(sig)
	fee := pos.Size.Mul(decimal.NewFromFloat(float64(e.feeRateBPS) / 10000.0))

	// Compute P&L.
	var pnl decimal.Decimal
	if pos.Side == strategy.BuyYes {
		pnl = closePrice.Sub(pos.EntryPrice).Mul(pos.Size)
	} else {
		pnl = pos.EntryPrice.Sub(closePrice).Mul(pos.Size)
	}
	pnl = pnl.Sub(fee)

	e.mu.Lock()
	e.balance = e.balance.Add(pos.Size).Add(pnl)
	e.mu.Unlock()

	e.risk.RecordClose(sig.StrategyID, sig.MarketID, pos.Size, pnl)

	// Update Kelly history.
	won := pnl.IsPositive()
	e.sizer.RecordTrade(sig.StrategyID, won, pnl)

	orderID := e.newOrderID()
	fill := Fill{
		OrderID:    orderID,
		StrategyID: sig.StrategyID,
		MarketID:   sig.MarketID,
		Side:       sig.Direction,
		Price:      closePrice,
		Size:       pos.Size,
		Fee:        fee,
		Timestamp:  sig.Timestamp,
	}
	e.mu.Lock()
	e.Fills = append(e.Fills, fill)
	e.mu.Unlock()

	e.ledger.ClosePosition(sig.StrategyID, sig.MarketID)
	e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
		StrategyID:  sig.StrategyID,
		MarketID:    sig.MarketID,
		Status:      strategy.StatusClosed,
		Side:        pos.Side,
		EntryPrice:  pos.EntryPrice,
		Size:        pos.Size,
		OpenOrderID: orderID,
		OrderState:  strategy.OrderFilled,
	})
}

// simulateFillPrice returns the fill price based on the fill model.
// In a real backtest, this would use the snapshot at signal time.
func (e *SimulatedExecutor) simulateFillPrice(sig strategy.Signal) decimal.Decimal {
	// Both fill models use 0.5 as a default since we don't carry market state here.
	// The runner should inject the current mid price for more accurate simulation.
	return decimal.NewFromFloat(0.5)
}

// CancelAll is a no-op in simulation (all orders fill immediately).
func (e *SimulatedExecutor) CancelAll(_ context.Context) error { return nil }

// CloseAll closes all remaining open positions in simulation.
func (e *SimulatedExecutor) CloseAll(_ context.Context) error {
	positions := e.ledger.OpenPositions()
	for _, pos := range positions {
		sig := strategy.Signal{
			StrategyID: pos.StrategyID,
			MarketID:   pos.MarketID,
			Direction:  strategy.Close,
			Timestamp:  time.Now(),
		}
		e.simulateClose(sig)
	}
	return nil
}

// Positions returns all tracked positions.
func (e *SimulatedExecutor) Positions() []Position {
	return e.ledger.AllPositions()
}

// Balance returns the current simulated balance.
func (e *SimulatedExecutor) Balance() decimal.Decimal {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.balance
}

func (e *SimulatedExecutor) sendFeedback(strategyID string, update strategy.PositionUpdate) {
	e.mu.Lock()
	ch, ok := e.feedbackChs[strategyID]
	e.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- update:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- update:
		default:
		}
	}
}

func (e *SimulatedExecutor) recordRejection(sig strategy.Signal, reason string) {
	e.mu.Lock()
	e.RejectedSignals = append(e.RejectedSignals, RejectedSignal{Signal: sig, Reason: reason})
	e.mu.Unlock()
	e.log.Debug().
		Str("strategy", sig.StrategyID).
		Str("market", sig.MarketID).
		Str("reason", reason).
		Msg("signal rejected")
}

func (e *SimulatedExecutor) newOrderID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextOrderID++
	return fmt.Sprintf("sim-%d", e.nextOrderID)
}

// ensure SimulatedExecutor satisfies Executor interface at compile time
var _ Executor = (*SimulatedExecutor)(nil)

package backtest

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/executor"
	"polysnipe/internal/strategy"
)

// Reporter generates backtest output files.
type Reporter struct {
	outputDir string
	log       zerolog.Logger
}

// NewReporter creates a reporter that writes to outputDir.
func NewReporter(outputDir string, log zerolog.Logger) *Reporter {
	return &Reporter{outputDir: outputDir, log: log.With().Str("component", "reporter").Logger()}
}

// ExecutionRecord is one row in executions.json.
type ExecutionRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	StrategyID string    `json:"strategy_id"`
	MarketID   string    `json:"market_id"`
	Side       string    `json:"side"`
	Price      string    `json:"price"`
	Size       string    `json:"size"`
	Fee        string    `json:"fee"`
	OrderID    string    `json:"order_id"`
}

// SignalRecord is one row in signals.json.
type SignalRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	StrategyID string    `json:"strategy_id"`
	MarketID   string    `json:"market_id"`
	Direction  string    `json:"direction"`
	Rejected   bool      `json:"rejected"`
	Reason     string    `json:"reason,omitempty"`
}

// StrategyStats holds per-strategy summary statistics.
type StrategyStats struct {
	StrategyID    string  `json:"strategy_id"`
	TotalTrades   int     `json:"total_trades"`
	Wins          int     `json:"wins"`
	Losses        int     `json:"losses"`
	WinRate       float64 `json:"win_rate"`
	GrossPnL      string  `json:"gross_pnl"`
	TotalFees     string  `json:"total_fees"`
	NetPnL        string  `json:"net_pnl"`
	ProfitFactor  float64 `json:"profit_factor"`
	MaxDrawdown   string  `json:"max_drawdown"`
	SharpeRatio   float64 `json:"sharpe_ratio"`
}

// Summary is the backtest summary report.
type Summary struct {
	StartDate       string          `json:"start_date"`
	EndDate         string          `json:"end_date"`
	StartingBalance string          `json:"starting_balance"`
	EndingBalance   string          `json:"ending_balance"`
	TotalTrades     int             `json:"total_trades"`
	WinRate         float64         `json:"win_rate"`
	GrossPnL        string          `json:"gross_pnl"`
	TotalFees       string          `json:"total_fees"`
	NetPnL          string          `json:"net_pnl"`
	ProfitFactor    float64         `json:"profit_factor"`
	MaxDrawdown     string          `json:"max_drawdown"`
	MaxDrawdownDur  string          `json:"max_drawdown_duration"`
	SharpeRatio     float64         `json:"sharpe_ratio"`
	PerStrategy     []StrategyStats `json:"per_strategy"`
}

// Write generates all backtest output files.
func (r *Reporter) Write(sim *executor.SimulatedExecutor, startingBalance decimal.Decimal, start, end time.Time) error {
	if err := os.MkdirAll(r.outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	fills := sim.Fills()
	rejected := sim.RejectedSignals
	endBalance := sim.Balance()

	// Write executions.json.
	execRecords := make([]ExecutionRecord, 0, len(fills))
	for _, f := range fills {
		execRecords = append(execRecords, ExecutionRecord{
			Timestamp:  f.Timestamp,
			StrategyID: f.StrategyID,
			MarketID:   f.MarketID,
			Side:       f.Side.String(),
			Price:      f.Price.String(),
			Size:       f.Size.String(),
			Fee:        f.Fee.String(),
			OrderID:    f.OrderID,
		})
	}
	if err := writeJSON(filepath.Join(r.outputDir, "executions.json"), execRecords); err != nil {
		return err
	}

	// Write signals.json.
	sigRecords := make([]SignalRecord, 0, len(fills)+len(rejected))
	for _, f := range fills {
		sigRecords = append(sigRecords, SignalRecord{
			Timestamp:  f.Timestamp,
			StrategyID: f.StrategyID,
			MarketID:   f.MarketID,
			Direction:  f.Side.String(),
			Rejected:   false,
		})
	}
	for _, rs := range rejected {
		dir := "unknown"
		switch rs.Signal.Direction {
		case strategy.BuyYes:
			dir = "BuyYes"
		case strategy.BuyNo:
			dir = "BuyNo"
		case strategy.Close:
			dir = "Close"
		}
		sigRecords = append(sigRecords, SignalRecord{
			Timestamp:  rs.Signal.Timestamp,
			StrategyID: rs.Signal.StrategyID,
			MarketID:   rs.Signal.MarketID,
			Direction:  dir,
			Rejected:   true,
			Reason:     rs.Reason,
		})
	}
	if err := writeJSON(filepath.Join(r.outputDir, "signals.json"), sigRecords); err != nil {
		return err
	}

	// Compute summary stats.
	summary := r.computeSummary(fills, startingBalance, endBalance, start, end)
	if err := writeJSON(filepath.Join(r.outputDir, "summary.json"), summary); err != nil {
		return err
	}

	r.log.Info().
		Str("output_dir", r.outputDir).
		Int("total_trades", summary.TotalTrades).
		Float64("win_rate", summary.WinRate).
		Str("net_pnl", summary.NetPnL).
		Float64("sharpe", summary.SharpeRatio).
		Msg("backtest report written")

	return nil
}

func (r *Reporter) computeSummary(fills []executor.Fill, startBal, endBal decimal.Decimal, start, end time.Time) Summary {
	// Compute trade pairs: entry + close = 1 trade.
	// Since fills include both entries and closes, count pairs.
	type tradePair struct {
		strategyID string
		marketID   string
		entryPrice decimal.Decimal
		closePrice decimal.Decimal
		size       decimal.Decimal
		fees       decimal.Decimal
		pnl        decimal.Decimal
		won        bool
	}

	// Match fills in pairs (every 2 fills for the same strategy+market = 1 round trip).
	openByKey := make(map[string]executor.Fill)
	var trades []tradePair

	for _, f := range fills {
		key := f.StrategyID + ":" + f.MarketID
		if _, open := openByKey[key]; !open {
			openByKey[key] = f
		} else {
			entry := openByKey[key]
			delete(openByKey, key)
			var pnl decimal.Decimal
			// BuyYes entry: profit = (close_price - entry_price) * size
			pnl = f.Price.Sub(entry.Price).Mul(entry.Size)
			totalFees := entry.Fee.Add(f.Fee)
			netPnL := pnl.Sub(totalFees)
			trades = append(trades, tradePair{
				strategyID: f.StrategyID,
				marketID:   f.MarketID,
				entryPrice: entry.Price,
				closePrice: f.Price,
				size:       entry.Size,
				fees:       totalFees,
				pnl:        netPnL,
				won:        netPnL.IsPositive(),
			})
		}
	}

	totalTrades := len(trades)
	wins := 0
	totalGross := decimal.Zero
	totalFees := decimal.Zero
	totalWin := decimal.Zero
	totalLoss := decimal.Zero

	// Running balance for drawdown.
	balance := startBal
	peak := startBal
	maxDD := decimal.Zero

	var pnls []float64
	for _, t := range trades {
		if t.won {
			wins++
			totalWin = totalWin.Add(t.pnl.Abs())
		} else {
			totalLoss = totalLoss.Add(t.pnl.Abs())
		}
		totalGross = totalGross.Add(t.pnl)
		totalFees = totalFees.Add(t.fees)
		balance = balance.Add(t.pnl)
		if balance.GreaterThan(peak) {
			peak = balance
		}
		dd := peak.Sub(balance)
		if dd.GreaterThan(maxDD) {
			maxDD = dd
		}
		pnls = append(pnls, t.pnl.InexactFloat64())
	}

	netPnL := endBal.Sub(startBal)
	var winRate float64
	if totalTrades > 0 {
		winRate = float64(wins) / float64(totalTrades)
	}

	var profitFactor float64
	if totalLoss.IsPositive() {
		profitFactor, _ = totalWin.Div(totalLoss).Float64()
	}

	sharpe := computeSharpe(pnls)

	// Per-strategy breakdown.
	stratMap := make(map[string]*StrategyStats)
	for _, t := range trades {
		ss := stratMap[t.strategyID]
		if ss == nil {
			ss = &StrategyStats{StrategyID: t.strategyID}
			stratMap[t.strategyID] = ss
		}
		ss.TotalTrades++
		if t.won {
			ss.Wins++
		} else {
			ss.Losses++
		}
	}
	var perStrategy []StrategyStats
	for _, ss := range stratMap {
		if ss.TotalTrades > 0 {
			ss.WinRate = float64(ss.Wins) / float64(ss.TotalTrades)
		}
		perStrategy = append(perStrategy, *ss)
	}

	return Summary{
		StartDate:       start.Format(time.DateOnly),
		EndDate:         end.Format(time.DateOnly),
		StartingBalance: startBal.String(),
		EndingBalance:   endBal.String(),
		TotalTrades:     totalTrades,
		WinRate:         winRate,
		GrossPnL:        totalGross.String(),
		TotalFees:       totalFees.String(),
		NetPnL:          netPnL.String(),
		ProfitFactor:    profitFactor,
		MaxDrawdown:     maxDD.String(),
		MaxDrawdownDur:  "N/A",
		SharpeRatio:     sharpe,
		PerStrategy:     perStrategy,
	}
}

// computeSharpe computes annualized Sharpe ratio (assuming daily P&L, risk-free = 0).
func computeSharpe(pnls []float64) float64 {
	if len(pnls) < 2 {
		return 0
	}
	mean := 0.0
	for _, p := range pnls {
		mean += p
	}
	mean /= float64(len(pnls))

	variance := 0.0
	for _, p := range pnls {
		d := p - mean
		variance += d * d
	}
	variance /= float64(len(pnls) - 1)
	std := math.Sqrt(variance)

	if std == 0 {
		return 0
	}
	return (mean / std) * math.Sqrt(252)
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return os.WriteFile(path, data, 0644)
}

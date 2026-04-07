package risk

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
	"polysnipe/internal/strategy"
)

// Manager enforces risk limits at three levels: global, per-market, and per-strategy.
type Manager struct {
	cfg config.RiskConfig
	log zerolog.Logger

	mu              sync.Mutex
	totalExposure   decimal.Decimal
	sessionPnL      decimal.Decimal
	peakBalance     decimal.Decimal
	dailyTrades     int
	lastTradeDate   time.Time
	halted          bool

	// per-market exposure and active strategy counts
	marketExposure  map[string]decimal.Decimal
	marketActive    map[string]int // number of strategies with open positions
}

// NewManager creates a new risk manager.
func NewManager(cfg config.RiskConfig, startingBalance decimal.Decimal, log zerolog.Logger) *Manager {
	return &Manager{
		cfg:            cfg,
		log:            log,
		peakBalance:    startingBalance,
		marketExposure: make(map[string]decimal.Decimal),
		marketActive:   make(map[string]int),
		lastTradeDate:  time.Now(),
	}
}

// Check validates a signal before order placement. Returns nil if allowed.
func (m *Manager) Check(sig strategy.Signal, strategyID string, size decimal.Decimal) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.halted {
		return fmt.Errorf("trading is halted (session loss limit or drawdown exceeded)")
	}

	// Reset daily trade count if it's a new day.
	now := time.Now()
	if m.lastTradeDate.Day() != now.Day() || m.lastTradeDate.Month() != now.Month() {
		m.dailyTrades = 0
		m.lastTradeDate = now
	}

	// Close signals bypass most checks.
	if sig.Direction == strategy.Close {
		return nil
	}

	// --- Global limits ---
	globalCfg := m.cfg.Global
	if globalCfg.MaxDailyTrades > 0 && m.dailyTrades >= globalCfg.MaxDailyTrades {
		return fmt.Errorf("global max_daily_trades (%d) reached", globalCfg.MaxDailyTrades)
	}
	maxTotal := decimal.NewFromFloat(globalCfg.MaxTotalExposure)
	if m.totalExposure.Add(size).GreaterThan(maxTotal) {
		return fmt.Errorf("global max_total_exposure (%.2f) would be exceeded; current=%.2f size=%.2f",
			globalCfg.MaxTotalExposure, m.totalExposure.InexactFloat64(), size.InexactFloat64())
	}

	// --- Per-market limits ---
	mktLimits := m.marketLimits(sig.MarketID)
	mktExp := m.marketExposure[sig.MarketID]
	maxMktExp := decimal.NewFromFloat(mktLimits.MaxExposure)
	if mktExp.Add(size).GreaterThan(maxMktExp) {
		return fmt.Errorf("market %s max_exposure (%.2f) would be exceeded; current=%.2f size=%.2f",
			sig.MarketID, mktLimits.MaxExposure, mktExp.InexactFloat64(), size.InexactFloat64())
	}
	if mktLimits.MaxStrategiesActive > 0 && m.marketActive[sig.MarketID] >= mktLimits.MaxStrategiesActive {
		return fmt.Errorf("market %s max_strategies_active (%d) reached", sig.MarketID, mktLimits.MaxStrategiesActive)
	}

	// --- Per-strategy limits ---
	stratLimits := m.strategyLimits(strategyID)
	maxPos := decimal.NewFromFloat(stratLimits.MaxPositionSize)
	if size.GreaterThan(maxPos) {
		return fmt.Errorf("strategy %s max_position_size (%.2f) exceeded; size=%.2f",
			strategyID, stratLimits.MaxPositionSize, size.InexactFloat64())
	}

	return nil
}

// RecordOpen updates exposure tracking when a position is opened.
func (m *Manager) RecordOpen(strategyID, marketID string, size decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalExposure = m.totalExposure.Add(size)
	m.marketExposure[marketID] = m.marketExposure[marketID].Add(size)
	m.marketActive[marketID]++
	m.dailyTrades++
}

// RecordClose updates exposure tracking and P&L when a position is closed.
func (m *Manager) RecordClose(strategyID, marketID string, size, pnl decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalExposure = m.totalExposure.Sub(size)
	if m.totalExposure.IsNegative() {
		m.totalExposure = decimal.Zero
	}
	mktExp := m.marketExposure[marketID].Sub(size)
	if mktExp.IsNegative() {
		mktExp = decimal.Zero
	}
	m.marketExposure[marketID] = mktExp
	if m.marketActive[marketID] > 0 {
		m.marketActive[marketID]--
	}

	m.sessionPnL = m.sessionPnL.Add(pnl)

	// Check session loss limit.
	lossLimit := decimal.NewFromFloat(m.cfg.Global.SessionLossLimit)
	if m.sessionPnL.LessThan(lossLimit) {
		m.halted = true
		m.log.Error().Str("session_pnl", m.sessionPnL.String()).Msg("session loss limit exceeded; trading halted")
	}

	// Check max drawdown.
	currentBalance := m.peakBalance.Add(m.sessionPnL)
	if currentBalance.GreaterThan(m.peakBalance) {
		m.peakBalance = currentBalance
	}
	if m.cfg.Global.MaxDrawdownPct > 0 && m.peakBalance.IsPositive() {
		drawdown := m.peakBalance.Sub(currentBalance).Div(m.peakBalance).Mul(decimal.NewFromInt(100))
		if drawdown.InexactFloat64() >= m.cfg.Global.MaxDrawdownPct {
			m.halted = true
			m.log.Error().Str("drawdown_pct", drawdown.String()).Msg("max drawdown exceeded; trading halted")
		}
	}
}

// IsHalted returns true if trading is halted.
func (m *Manager) IsHalted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.halted
}

// Resume clears the halt flag (manual override).
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.halted = false
}

func (m *Manager) marketLimits(marketID string) config.MarketRiskLimits {
	if override, ok := m.cfg.PerMarket.Overrides[marketID]; ok {
		return override
	}
	return m.cfg.PerMarket.Default
}

func (m *Manager) strategyLimits(strategyID string) config.StrategyRiskLimits {
	if override, ok := m.cfg.PerStrategy.Overrides[strategyID]; ok {
		return override
	}
	return m.cfg.PerStrategy.Default
}

// SessionPnL returns the current session P&L.
func (m *Manager) SessionPnL() decimal.Decimal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionPnL
}

// TotalExposure returns the current total exposure.
func (m *Manager) TotalExposure() decimal.Decimal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalExposure
}

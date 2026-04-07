package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"polysnipe/internal/backtest"
	"polysnipe/internal/config"
	"polysnipe/internal/dashboard"
	"polysnipe/internal/executor"
	"polysnipe/internal/feed"
	"polysnipe/internal/risk"
	"polysnipe/internal/sizing"
	"polysnipe/internal/state"
	"polysnipe/internal/strategy"
)

// strategyFactory maps strategy names from config to their constructors.
var strategyFactory = map[string]func(id string) strategy.Strategy{
	"spread_threshold":     func(id string) strategy.Strategy { return strategy.NewSpreadThreshold(id) },
	"momentum":             func(id string) strategy.Strategy { return strategy.NewMomentum(id) },
	"time_decay":           func(id string) strategy.Strategy { return strategy.NewTimeDecay(id) },
	"mid_flip":             func(id string) strategy.Strategy { return strategy.NewMidFlip(id) },
	"reversal_snipe":       func(id string) strategy.Strategy { return strategy.NewReversalSnipe(id) },
	"last_second_collapse": func(id string) strategy.Strategy { return strategy.NewLastSecondCollapse(id) },
	"strategy_7":           func(id string) strategy.Strategy { return strategy.NewStrategy7(id) },
	"strategy_8":           func(id string) strategy.Strategy { return strategy.NewStrategy8(id) },
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// Load config.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config validation: %v\n", err)
		os.Exit(1)
	}

	// Set up logger.
	logger := buildLogger(cfg.Logging)
	log.Logger = logger

	logger.Info().Str("config", *cfgPath).Msg("PolySnipe starting")

	if cfg.Backtest.Enabled {
		runBacktest(cfg, logger)
		return
	}
	runLive(cfg, logger)
}

// --- Backtest mode ---

func runBacktest(cfg *config.Config, logger zerolog.Logger) {
	logger.Info().Msg("running in backtest mode")

	strategies, err := buildStrategies(cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("build strategies")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	runner := backtest.NewRunner(cfg, logger)
	if err := runner.Run(ctx, strategies); err != nil {
		logger.Fatal().Err(err).Msg("backtest failed")
	}
	logger.Info().Str("output", cfg.Backtest.OutputDir).Msg("backtest complete")
}

// --- Live trading mode ---

type orchestrator struct {
	cfg       *config.Config
	startTime time.Time
	lastErr   string

	mu              sync.Mutex
	pausedStrategies map[string]bool
	exec            executor.Executor
	riskMgr         *risk.Manager
}

func (o *orchestrator) Positions() []executor.Position { return o.exec.Positions() }
func (o *orchestrator) SessionPnL() float64 {
	v, _ := o.riskMgr.SessionPnL().Float64()
	return v
}
func (o *orchestrator) IsHalted() bool { return o.riskMgr.IsHalted() }
func (o *orchestrator) ResumeTrading() { o.riskMgr.Resume() }
func (o *orchestrator) PauseStrategy(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pausedStrategies[id] = true
}
func (o *orchestrator) ResumeStrategy(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.pausedStrategies, id)
}
func (o *orchestrator) Uptime() time.Duration { return time.Since(o.startTime) }
func (o *orchestrator) GoroutineCount() int   { return runtime.NumGoroutine() }
func (o *orchestrator) LastError() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastErr
}

func runLive(cfg *config.Config, logger zerolog.Logger) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	balance := decimal.NewFromFloat(1000.0) // TODO: fetch from wallet

	riskMgr := risk.NewManager(cfg.Risk, balance, logger)

	var sizer sizing.Sizer
	if cfg.Sizing.Method == "kelly" {
		sizer = sizing.NewKellySizer(cfg.Sizing)
	} else {
		sizer = sizing.NewFixedSizer(cfg.Sizing)
	}

	liveExec, err := executor.NewLiveExecutor(cfg.Execution, cfg.Connection, riskMgr, sizer, balance, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("create executor")
	}

	orch := &orchestrator{
		cfg:              cfg,
		startTime:        time.Now(),
		pausedStrategies: make(map[string]bool),
		exec:             liveExec,
		riskMgr:          riskMgr,
	}

	strategies, err := buildStrategies(cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("build strategies")
	}

	// Build channels.
	signalCh := make(chan strategy.Signal, 16)
	feedbackChs := make(map[string]chan strategy.PositionUpdate)
	for _, strat := range strategies {
		feedbackChs[strat.ID()] = make(chan strategy.PositionUpdate, 1)
	}

	// Build market feed channels and state engines.
	marketFeedChs := make(map[string]chan feed.MarketEvent)
	engines := make(map[string]*state.Engine)
	windowEnd := time.Now().Add(5 * time.Minute) // rough default; update per market

	for _, mktCfg := range cfg.Markets {
		ch := make(chan feed.MarketEvent, 64)
		marketFeedChs[mktCfg.ID] = ch

		eng := state.NewEngine(mktCfg.ID, windowEnd, logger)
		engines[mktCfg.ID] = eng

		// Subscribe each strategy that cares about this market.
		for _, strat := range strategies {
			for _, mID := range strat.Markets() {
				if mID == mktCfg.ID {
					snapCh := make(chan state.MarketSnapshot, 1)
					eng.Subscribe(snapCh)
					// Run strategy goroutine for this market.
					go strat.Run(ctx, snapCh, feedbackChs[strat.ID()], signalCh)
					break
				}
			}
		}
	}

	var wg sync.WaitGroup

	// Start executor.
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveExec.Run(ctx, signalCh, feedbackChs)
	}()

	// Start state engines.
	for mktID, eng := range engines {
		wg.Add(1)
		go func(e *state.Engine, ch <-chan feed.MarketEvent) {
			defer wg.Done()
			e.Run(ctx, ch)
		}(eng, marketFeedChs[mktID])
	}

	// Start WebSocket feeds.
	reconnectCfg := feed.ReconnectConfig{
		MaxRetries:    cfg.Connection.ReconnectMaxRetries,
		BackoffBaseMS: cfg.Connection.ReconnectBackoffBaseMS,
		BackoffMaxMS:  cfg.Connection.ReconnectBackoffMaxMS,
	}
	for _, mktCfg := range cfg.Markets {
		mktCfg := mktCfg
		tokenIDs := []string{mktCfg.TokenIDYes, mktCfg.TokenIDNo}
		wsFeed := feed.NewWSFeed(cfg.Connection.WebSocketURL, mktCfg.ID, tokenIDs, reconnectCfg, marketFeedChs[mktCfg.ID], logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			wsFeed.Run(ctx)
		}()
	}

	// Start dashboard.
	if cfg.Dashboard.Enabled {
		dash := dashboard.NewServer(cfg.Dashboard, riskMgr, orch, logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dash.Run(ctx); err != nil {
				logger.Error().Err(err).Msg("dashboard error")
			}
		}()
	}

	logger.Info().Int("strategies", len(strategies)).Int("markets", len(cfg.Markets)).Msg("bot running")

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info().Msg("shutdown signal received; beginning graceful shutdown")

	// Shutdown sequence (spec §Shutdown Sequence).
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()

	if err := liveExec.CancelAll(shutCtx); err != nil {
		logger.Error().Err(err).Msg("cancel all orders")
	}
	if err := liveExec.CloseAll(shutCtx); err != nil {
		logger.Error().Err(err).Msg("close all positions")
	}

	wg.Wait()
	logger.Info().Msg("PolySnipe stopped cleanly")
}

// buildStrategies instantiates enabled strategies from config.
func buildStrategies(cfg *config.Config) ([]strategy.Strategy, error) {
	var strategies []strategy.Strategy
	for name, stratCfg := range cfg.Strategies {
		if !stratCfg.Enabled {
			continue
		}
		factory, ok := strategyFactory[name]
		if !ok {
			return nil, fmt.Errorf("unknown strategy: %s", name)
		}
		strat := factory(name)
		// Set markets.
		if err := strat.Configure(stratCfg.Params); err != nil {
			return nil, fmt.Errorf("configure strategy %s: %w", name, err)
		}
		// We need to set the markets on the strategy.
		// The interface doesn't have a SetMarkets method; we handle this through Configure.
		// Each concrete type stores markets — inject via the Markets() return slice by calling
		// a type-assertion pattern or via a setter interface.
		if ms, ok := strat.(interface{ SetMarkets([]string) }); ok {
			ms.SetMarkets(stratCfg.Markets)
		}
		strategies = append(strategies, strat)
	}
	return strategies, nil
}

func buildLogger(cfg config.LoggingConfig) zerolog.Logger {
	var w io.Writer = os.Stdout
	if cfg.Format != "json" {
		w = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	}
	if err := os.MkdirAll(cfg.OutputDir, 0755); err == nil && cfg.OutputDir != "" {
		logFile, err := os.OpenFile(cfg.OutputDir+"/polysnipe.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			w = io.MultiWriter(w, logFile)
		}
	}
	level := zerolog.InfoLevel
	switch cfg.Level {
	case "debug":
		level = zerolog.DebugLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	}
	return zerolog.New(w).Level(level).With().Timestamp().Logger()
}

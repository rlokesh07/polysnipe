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
	"polysnipe/internal/discovery"
	"polysnipe/internal/executor"
	"polysnipe/internal/feed"
	"polysnipe/internal/gamma"
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
	"sma_reversion":        func(id string) strategy.Strategy { return strategy.NewSMAReversion(id) },
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config validation: %v\n", err)
		os.Exit(1)
	}

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

// MarketStack holds all goroutine state for a single discovered market.
type MarketStack struct {
	MarketID     string
	Question     string
	Tags         []string
	Strategies   []string
	SubscribedAt time.Time
	Cancel       context.CancelFunc
	Done         chan struct{}

	priceMu      sync.Mutex
	lastPrice    float64
	bestBid      float64
	bestAsk      float64
	lastTick     time.Time // zero until first price update
	priceHistory []float64 // rolling mid-price history, capped at 300
}

const maxPriceHistory = 300

func (ms *MarketStack) setPrice(snap state.MarketSnapshot) {
	ms.priceMu.Lock()
	ms.lastPrice, _ = snap.LastPrice.Float64()
	ms.bestBid, _ = snap.BestBid.Float64()
	ms.bestAsk, _ = snap.BestAsk.Float64()
	ms.lastTick = time.Now()

	mid := 0.0
	if ms.bestBid > 0 && ms.bestAsk > 0 {
		mid = (ms.bestBid + ms.bestAsk) / 2
	} else {
		mid = ms.lastPrice
	}
	if mid > 0 {
		ms.priceHistory = append(ms.priceHistory, mid)
		if len(ms.priceHistory) > maxPriceHistory {
			ms.priceHistory = ms.priceHistory[len(ms.priceHistory)-maxPriceHistory:]
		}
	}
	ms.priceMu.Unlock()
}

func (ms *MarketStack) getPriceHistory() []float64 {
	ms.priceMu.Lock()
	defer ms.priceMu.Unlock()
	out := make([]float64, len(ms.priceHistory))
	copy(out, ms.priceHistory)
	return out
}

// timeSinceLastTick returns how long ago the last price update was received.
// If no tick has arrived yet, it measures from SubscribedAt.
func (ms *MarketStack) timeSinceLastTick() time.Duration {
	ms.priceMu.Lock()
	defer ms.priceMu.Unlock()
	if ms.lastTick.IsZero() {
		return time.Since(ms.SubscribedAt)
	}
	return time.Since(ms.lastTick)
}

func (ms *MarketStack) getPrice() (lastPrice, bestBid, bestAsk float64) {
	ms.priceMu.Lock()
	defer ms.priceMu.Unlock()
	return ms.lastPrice, ms.bestBid, ms.bestAsk
}

// orchestrator manages the dynamic market lifecycle.
type orchestrator struct {
	cfg       *config.Config
	startTime time.Time
	lastErr   string

	mu               sync.Mutex
	pausedStrategies map[string]bool
	exec             executor.Executor
	riskMgr          *risk.Manager

	// dynamic market registry
	registryMu sync.RWMutex
	registry   map[string]*MarketStack // conditionID → stack
	questions  map[string]string       // conditionID → human-readable question
}

func (o *orchestrator) Positions() []executor.Position { return o.exec.Positions() }

func (o *orchestrator) hasActivePosition(marketID string) bool {
	for _, p := range o.exec.Positions() {
		if p.MarketID == marketID {
			return true
		}
	}
	return false
}
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
func (o *orchestrator) Balance() float64 {
	switch ex := o.exec.(type) {
	case *executor.SimulatedExecutor:
		v, _ := ex.Balance().Float64()
		return v
	case *executor.LiveExecutor:
		v, _ := ex.Balance().Float64()
		return v
	}
	return 0
}

func (o *orchestrator) RecentFills() []executor.Fill {
	sim, ok := o.exec.(*executor.SimulatedExecutor)
	if !ok {
		return nil
	}
	fills := sim.Fills()
	o.registryMu.RLock()
	defer o.registryMu.RUnlock()
	for i := range fills {
		if q, ok := o.questions[fills[i].MarketID]; ok {
			fills[i].Question = q
		}
	}
	return fills
}

func (o *orchestrator) IsDryRun() bool { return o.cfg.DryRun }

func (o *orchestrator) ActiveMarkets() []dashboard.MarketStackInfo {
	o.registryMu.RLock()
	defer o.registryMu.RUnlock()
	out := make([]dashboard.MarketStackInfo, 0, len(o.registry))
	for _, ms := range o.registry {
		lastPrice, bestBid, bestAsk := ms.getPrice()
		out = append(out, dashboard.MarketStackInfo{
			MarketID:     ms.MarketID,
			Question:     ms.Question,
			Tags:         ms.Tags,
			Strategies:   ms.Strategies,
			SubscribedAt: ms.SubscribedAt,
			LastPrice:    lastPrice,
			BestBid:      bestBid,
			BestAsk:      bestAsk,
			PriceHistory: ms.getPriceHistory(),
		})
	}
	return out
}

func runLive(cfg *config.Config, logger zerolog.Logger) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	balance := decimal.NewFromFloat(cfg.DryRunBalance) // set via dry_run_balance in config.yaml

	riskMgr := risk.NewManager(cfg.Risk, balance, logger)

	var sizer sizing.Sizer
	if cfg.Sizing.Method == "kelly" {
		sizer = sizing.NewKellySizer(cfg.Sizing)
	} else {
		sizer = sizing.NewFixedSizer(cfg.Sizing)
	}

	// marketStates holds the latest bid/ask and YES token ID for each subscribed market.
	// Keyed by conditionID; updated by subscribeMarket; read by LiveExecutor for order pricing and signing.
	type marketStateEntry struct {
		bid, ask   decimal.Decimal
		tokenIDYes string // YES token uint256 ID (decimal string) — required for CTF Exchange orders
		negRisk    bool
	}
	var marketStates sync.Map

	getMarketState := func(marketID string) (bid, ask decimal.Decimal, tokenIDYes string, ok bool) {
		if v, ok := marketStates.Load(marketID); ok {
			s := v.(marketStateEntry)
			return s.bid, s.ask, s.tokenIDYes, s.bid.IsPositive() && s.ask.IsPositive()
		}
		return decimal.Zero, decimal.Zero, "", false
	}

	isNegRisk := func(marketID string) bool {
		if v, ok := marketStates.Load(marketID); ok {
			return v.(marketStateEntry).negRisk
		}
		return false
	}

	var liveExec executor.Executor
	if cfg.DryRun {
		logger.Warn().Msg("DRY RUN mode: using simulated executor — no real orders will be placed")
		liveExec = executor.NewSimulatedExecutor(
			cfg.Execution,
			cfg.Sizing,
			riskMgr,
			sizer,
			balance,
			cfg.Backtest.FeeRateBPS, // fee simulation (set fee_rate_bps in backtest config)
			"midpoint",              // fill at mid price
			logger,
		)
	} else {
		live, err := executor.NewLiveExecutor(cfg.Execution, cfg.Connection, riskMgr, sizer, balance, getMarketState, isNegRisk, logger)
		if err != nil {
			logger.Fatal().Err(err).Msg("create executor")
		}
		liveExec = live
	}

	orch := &orchestrator{
		cfg:              cfg,
		startTime:        time.Now(),
		pausedStrategies: make(map[string]bool),
		exec:             liveExec,
		riskMgr:          riskMgr,
		registry:         make(map[string]*MarketStack),
		questions:        make(map[string]string),
	}

	strategies, err := buildStrategies(cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("build strategies")
	}

	// Shared signal channel for all markets.
	signalCh := make(chan strategy.Signal, 64)

	// Feedback channels per strategy (live for the bot lifetime).
	feedbackChs := make(map[string]chan strategy.PositionUpdate)
	for _, strat := range strategies {
		feedbackChs[strat.ID()] = make(chan strategy.PositionUpdate, 4)
	}

	var wg sync.WaitGroup

	// Start executor.
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveExec.Run(ctx, signalCh, feedbackChs)
	}()

	reconnectCfg := feed.ReconnectConfig{
		MaxRetries:    cfg.Connection.ReconnectMaxRetries,
		BackoffBaseMS: cfg.Connection.ReconnectBackoffBaseMS,
		BackoffMaxMS:  cfg.Connection.ReconnectBackoffMaxMS,
	}

	// Single shared WebSocket connection for all markets.
	sharedFeed := feed.NewSharedFeed(cfg.Connection.WebSocketURL, reconnectCfg, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sharedFeed.Run(ctx)
	}()

	// Declared here so subscribeMarket/unsubscribeMarket closures can reference it.
	var discoveryEngine *discovery.Engine

	// subscribeMarket spins up a state engine + strategy goroutines for one market.
	subscribeMarket := func(cmd discovery.MarketCommand) {
		mktID := cmd.Market.ConditionID
		mktCtx, mktCancel := context.WithCancel(ctx)
		done := make(chan struct{})

		stack := &MarketStack{
			MarketID:     mktID,
			Question:     cmd.Market.Question,
			Tags:         cmd.Market.Tags,
			Strategies:   cmd.Strategies,
			SubscribedAt: time.Now(),
			Cancel:       mktCancel,
			Done:         done,
		}

		orch.registryMu.Lock()
		orch.registry[mktID] = stack
		orch.questions[mktID] = cmd.Market.Question
		orch.registryMu.Unlock()

		// Rough window end — strategies update internally; this just seeds the engine.
		windowEnd := time.Now().Add(24 * time.Hour)
		eng := state.NewEngine(mktID, windowEnd, logger)

		// Price tracker: keep latest snapshot on the stack for the dashboard.
		// Also enforces max_spread_cents: drops the market on first valid bid/ask tick if spread is too wide.
		priceCh := make(chan state.MarketSnapshot, 1)
		eng.Subscribe(priceCh)
		// Pick spread threshold: use updown override if this market has the up-or-down tag.
		maxSpreadCents := cfg.Discovery.MaxSpreadCents
		for _, tag := range cmd.Market.Tags {
			if tag == "up-or-down" && cfg.Discovery.UpDownMaxSpreadCents > 0 {
				maxSpreadCents = cfg.Discovery.UpDownMaxSpreadCents
				break
			}
		}
		go func() {
			spreadChecked := false
			for snap := range priceCh {
				stack.setPrice(snap)
				if snap.BestBid.IsPositive() && snap.BestAsk.IsPositive() {
					marketStates.Store(mktID, marketStateEntry{
						bid:        snap.BestBid,
						ask:        snap.BestAsk,
						tokenIDYes: cmd.Market.TokenIDYes,
					negRisk:    cmd.Market.NegRisk,
					})
				}
				if maxSpreadCents > 0 && !spreadChecked && snap.BestBid.IsPositive() && snap.BestAsk.IsPositive() {
					spreadChecked = true
					if orch.hasActivePosition(mktID) {
						continue
					}
					diff := snap.BestAsk.Sub(snap.BestBid)
					if diff.IsNegative() {
						diff = diff.Neg()
					}
					spread, _ := diff.Mul(decimal.NewFromInt(100)).Float64()
					if spread > maxSpreadCents {
						logger.Info().
							Str("market_id", mktID).
							Str("question", cmd.Market.Question).
							Float64("spread_cents", spread).
							Float64("max_spread_cents", maxSpreadCents).
							Msg("spread too wide; dropping market")
						mktCancel()
						sharedFeed.Unsubscribe(mktID)
						if discoveryEngine != nil {
							discoveryEngine.Forget(mktID)
						}
						orch.registryMu.Lock()
						delete(orch.registry, mktID)
						delete(orch.questions, mktID)
						orch.registryMu.Unlock()
					}
				}
			}
		}()

		// Subscribe matching strategies.
		for _, stratID := range cmd.Strategies {
			var strat strategy.Strategy
			for _, s := range strategies {
				if s.ID() == stratID {
					strat = s
					break
				}
			}
			if strat == nil {
				continue
			}
			snapCh := make(chan state.MarketSnapshot, 4)
			eng.Subscribe(snapCh)
			fbCh := feedbackChs[stratID]
			go strat.Run(mktCtx, snapCh, fbCh, signalCh)
		}

		rawFeedCh := make(chan feed.MarketEvent, 64)
		sharedFeed.Subscribe(mktID, cmd.Market.TokenIDYes, cmd.Market.TokenIDNo, rawFeedCh)

		// Tee events to disk for future backtesting, then forward to state engine.
		feedCh := make(chan feed.MarketEvent, 64)
		if rec, err := feed.NewRecorder(cfg.Backtest.DataCacheDir, mktID); err != nil {
			logger.Warn().Err(err).Str("market_id", mktID).Msg("recorder failed; events will not be saved")
			go func() {
				for ev := range rawFeedCh {
					feedCh <- ev
				}
				close(feedCh)
			}()
		} else {
			logger.Info().Str("path", rec.Path()).Str("market_id", mktID).Msg("recording market events")
			go func() {
				for ev := range rawFeedCh {
					rec.Record(ev)
					feedCh <- ev
				}
				close(feedCh)
				rec.Close()
			}()
		}

		var mktWg sync.WaitGroup
		mktWg.Add(1)
		go func() {
			defer mktWg.Done()
			eng.Run(mktCtx, feedCh)
		}()

		// Close Done when all goroutines have exited.
		go func() {
			mktWg.Wait()
			close(done)
		}()

		logger.Info().
			Str("event", "market_subscribed").
			Str("market_id", mktID).
			Str("question", cmd.Market.Question).
			Strs("strategies_attached", cmd.Strategies).
			Msg("market subscribed")
	}

	// unsubscribeMarket tears down the goroutine stack for a market.
	unsubscribeMarket := func(marketID string) {
		orch.registryMu.Lock()
		stack, ok := orch.registry[marketID]
		if ok {
			delete(orch.registry, marketID)
			delete(orch.questions, marketID)
		}
		orch.registryMu.Unlock()
		marketStates.Delete(marketID)

		if !ok {
			return
		}

		stack.Cancel()
		sharedFeed.Unsubscribe(marketID)
		if discoveryEngine != nil {
			discoveryEngine.Forget(marketID)
		}

		// Attempt to close any open positions for this market.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if sim, ok := liveExec.(*executor.SimulatedExecutor); ok {
			lastPrice, _, _ := stack.getPrice()
			price := decimal.NewFromFloat(lastPrice)
			if err := sim.CloseMarketAtPrice(shutCtx, marketID, price); err != nil {
				logger.Error().Err(err).Str("market_id", marketID).Msg("close market positions on teardown")
			}
		} else if err := liveExec.CloseMarket(shutCtx, marketID); err != nil {
			logger.Error().Err(err).Str("market_id", marketID).Msg("close market positions on teardown")
		}

		select {
		case <-stack.Done:
		case <-time.After(15 * time.Second):
			logger.Warn().Str("market_id", marketID).Msg("market teardown timed out")
		}

		logger.Info().
			Str("event", "market_teardown").
			Str("market_id", marketID).
			Msg("market torn down")
	}

	// Stale market reaper: drops markets that haven't received a price tick within the timeout.
	if cfg.Discovery.StaleMarketTimeoutSec > 0 {
		defaultTimeout := time.Duration(cfg.Discovery.StaleMarketTimeoutSec) * time.Second
		upDownTimeout := defaultTimeout
		if cfg.Discovery.UpDownStaleTimeoutSec > 0 {
			upDownTimeout = time.Duration(cfg.Discovery.UpDownStaleTimeoutSec) * time.Second
		}

		staleTimeoutFor := func(stack *MarketStack) time.Duration {
			for _, tag := range stack.Tags {
				if tag == "up-or-down" {
					return upDownTimeout
				}
			}
			return defaultTimeout
		}

		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					orch.registryMu.RLock()
					var stale []string
					for id, stack := range orch.registry {
						if stack.timeSinceLastTick() > staleTimeoutFor(stack) && !orch.hasActivePosition(id) {
							stale = append(stale, id)
						}
					}
					orch.registryMu.RUnlock()
					for _, id := range stale {
						logger.Info().
							Str("market_id", id).
							Msg("no price updates; dropping stale market")
						unsubscribeMarket(id)
					}
				}
			}
		}()
	}

	// Start discovery engine if enabled.
	commandCh := make(chan discovery.MarketCommand, 16)

	if cfg.Discovery.Enabled {
		gammaClient := gamma.NewClient(cfg.Discovery.GammaAPIURL, cfg.Discovery.RateLimitPerSecond)
		watchlists := buildWatchlists(cfg)
		pollInterval := time.Duration(cfg.Discovery.PollIntervalSec) * time.Second
		if pollInterval <= 0 {
			pollInterval = 60 * time.Second
		}

		discoveryStrategies := make([]discovery.Strategy, len(strategies))
		for i, s := range strategies {
			discoveryStrategies[i] = s
		}

		var upDownAssets []string
		if cfg.Discovery.UpDownMarkets.Enabled {
			upDownAssets = cfg.Discovery.UpDownMarkets.Assets
		}
		discoveryEngine = discovery.NewEngine(gammaClient, watchlists, discoveryStrategies, commandCh, pollInterval, upDownAssets, logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			discoveryEngine.Run(ctx)
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

	logger.Info().
		Int("strategies", len(strategies)).
		Bool("discovery", cfg.Discovery.Enabled).
		Bool("dry_run", cfg.DryRun).
		Msg("bot running")

	// Main loop: read market commands from the discovery engine.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd, ok := <-commandCh:
				if !ok {
					return
				}
				switch cmd.Action {
				case discovery.Subscribe:
					orch.registryMu.RLock()
					_, already := orch.registry[cmd.Market.ConditionID]
					atCap := cfg.Discovery.MaxMarkets > 0 && len(orch.registry) >= cfg.Discovery.MaxMarkets
					orch.registryMu.RUnlock()
					if !already && !atCap {
						subscribeMarket(cmd)
					} else if !already && atCap {
						logger.Debug().
							Str("market_id", cmd.Market.ConditionID).
							Int("max_markets", cfg.Discovery.MaxMarkets).
							Msg("market cap reached; skipping new subscription")
					}
				case discovery.Unsubscribe:
					unsubscribeMarket(cmd.Market.ConditionID)
				}
			}
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info().Msg("shutdown signal received; beginning graceful shutdown")

	// Cancel all market stacks.
	orch.registryMu.Lock()
	for _, stack := range orch.registry {
		stack.Cancel()
	}
	orch.registryMu.Unlock()

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

// buildWatchlists converts config watchlists to discovery.Watchlist values.
func buildWatchlists(cfg *config.Config) []discovery.Watchlist {
	out := make([]discovery.Watchlist, 0, len(cfg.Discovery.Watchlists))
	for _, wl := range cfg.Discovery.Watchlists {
		out = append(out, discovery.Watchlist{
			Name: wl.Name,
			Tags: wl.Tags,
			Filters: discovery.PropertyFilters{
				MaxExpiryMinutes: wl.Filters.MaxExpiryMinutes,
				MinExpiryMinutes: wl.Filters.MinExpiryMinutes,
				OutcomeType:      wl.Filters.OutcomeType,
				MinVolume24h:     wl.Filters.MinVolume24h,
				MinLiquidity:     wl.Filters.MinLiquidity,
				Active:           wl.Filters.Active,
				TitleContains:    wl.Filters.TitleContains,
			},
		})
	}
	return out
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
		if err := strat.Configure(stratCfg.Params); err != nil {
			return nil, fmt.Errorf("configure strategy %s: %w", name, err)
		}
		if ts, ok := strat.(interface{ SetTags([]string) }); ok {
			ts.SetTags(stratCfg.Tags)
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

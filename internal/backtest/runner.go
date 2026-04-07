package backtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
	"polysnipe/internal/executor"
	"polysnipe/internal/feed"
	"polysnipe/internal/risk"
	"polysnipe/internal/sizing"
	"polysnipe/internal/state"
	"polysnipe/internal/strategy"
)

// Runner orchestrates a backtest run: fetches data, wires up the pipeline,
// runs strategies through the same code path as live trading, and produces a report.
type Runner struct {
	cfg      *config.Config
	fetcher  *Fetcher
	log      zerolog.Logger
	reporter *Reporter
}

// NewRunner creates a new backtest runner.
func NewRunner(cfg *config.Config, log zerolog.Logger) *Runner {
	fetcher := NewFetcher(cfg.Backtest.DataCacheDir, log)
	reporter := NewReporter(cfg.Backtest.OutputDir, log)
	return &Runner{
		cfg:      cfg,
		fetcher:  fetcher,
		log:      log.With().Str("component", "backtest_runner").Logger(),
		reporter: reporter,
	}
}

// Run executes the backtest and writes output files.
func (r *Runner) Run(ctx context.Context, strategies []strategy.Strategy) error {
	bt := r.cfg.Backtest

	start, err := time.Parse(time.DateOnly, bt.StartDate)
	if err != nil {
		return fmt.Errorf("parse start_date: %w", err)
	}
	end, err := time.Parse(time.DateOnly, bt.EndDate)
	if err != nil {
		return fmt.Errorf("parse end_date: %w", err)
	}

	r.log.Info().
		Str("start", bt.StartDate).
		Str("end", bt.EndDate).
		Float64("balance", bt.StartingBalance).
		Int("strategies", len(strategies)).
		Msg("starting backtest")

	balance := decimal.NewFromFloat(bt.StartingBalance)

	// Build risk manager.
	riskMgr := risk.NewManager(r.cfg.Risk, balance, r.log)

	// Build sizer.
	var sizer sizing.Sizer
	if r.cfg.Sizing.Method == "kelly" {
		sizer = sizing.NewKellySizer(r.cfg.Sizing)
	} else {
		sizer = sizing.NewFixedSizer(r.cfg.Sizing)
	}

	// Build simulated executor.
	sim := executor.NewSimulatedExecutor(
		r.cfg.Execution,
		r.cfg.Sizing,
		riskMgr,
		sizer,
		balance,
		bt.FeeRateBPS,
		bt.FillModel,
		r.log,
	)

	// Collect market IDs needed by the strategies.
	marketIDs := r.collectMarketIDs(strategies)

	// Fetch or generate historical data for each market.
	marketEvents := make(map[string][]feed.MarketEvent)
	for _, mktID := range marketIDs {
		mktCfg := r.cfg.MarketByID(mktID)
		var events []feed.MarketEvent
		if mktCfg != nil && mktCfg.TokenIDYes != "" {
			events, err = r.fetcher.Fetch(mktID, mktCfg.TokenIDYes, start, end)
			if err != nil {
				r.log.Warn().Err(err).Str("market", mktID).Msg("fetch failed; using synthetic data")
				events = GenerateSampleEvents(mktID, start, end)
			}
		} else {
			r.log.Info().Str("market", mktID).Msg("no token ID configured; using synthetic data")
			events = GenerateSampleEvents(mktID, start, end)
		}
		marketEvents[mktID] = events
		r.log.Info().Str("market", mktID).Int("events", len(events)).Msg("data ready")
	}

	// Determine total events for progress.
	totalEvents := 0
	for _, evs := range marketEvents {
		totalEvents += len(evs)
	}
	if totalEvents == 0 {
		return fmt.Errorf("no historical events available for any market")
	}

	// Build signal channel and per-strategy feedback channels.
	signalCh := make(chan strategy.Signal, 16)
	feedbackChs := make(map[string]chan strategy.PositionUpdate)
	for _, strat := range strategies {
		feedbackChs[strat.ID()] = make(chan strategy.PositionUpdate, 1)
	}

	// Build per-market state engines and subscribe strategies.
	windowEnd := end // use end of backtest as window end for simplicity
	engines := make(map[string]*state.Engine)
	snapshotChs := make(map[string]map[string]chan state.MarketSnapshot) // marketID -> stratID -> ch
	for _, mktID := range marketIDs {
		eng := state.NewEngine(mktID, windowEnd, r.log)
		engines[mktID] = eng
		snapshotChs[mktID] = make(map[string]chan state.MarketSnapshot)

		for _, strat := range strategies {
			for _, m := range strat.Markets() {
				if m == mktID {
					ch := make(chan state.MarketSnapshot, 1)
					eng.Subscribe(ch)
					snapshotChs[mktID][strat.ID()] = ch
					break
				}
			}
		}
	}

	backtestCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	// Start simulated executor.
	wg.Add(1)
	go func() {
		defer wg.Done()
		sim.Run(backtestCtx, signalCh, feedbackChs)
	}()

	// Start strategy goroutines — each strategy gets a merged snapshot channel.
	for _, strat := range strategies {
		strat := strat
		fbCh := feedbackChs[strat.ID()]
		// Merge all market snapshots for this strategy into one channel.
		mergedCh := make(chan state.MarketSnapshot, len(strat.Markets())+1)
		for _, mktID := range strat.Markets() {
			if ch, ok := snapshotChs[mktID][strat.ID()]; ok {
				src := ch
				go func() {
					for snap := range src {
						select {
						case mergedCh <- snap:
						default:
							select {
							case <-mergedCh:
							default:
							}
							select {
							case mergedCh <- snap:
							default:
							}
						}
					}
				}()
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			strat.Run(backtestCtx, mergedCh, fbCh, signalCh)
		}()
	}

	// Replay events through state engines sequentially (time-ordered across markets).
	allEvents := r.mergeAndSort(marketEvents)
	for _, ev := range allEvents {
		if ctx.Err() != nil {
			break
		}
		eng := engines[ev.MarketID]
		if eng == nil {
			continue
		}
		// Feed event directly into engine (no goroutine, synchronous for backtest).
		inCh := make(chan feed.MarketEvent, 1)
		inCh <- ev
		close(inCh)
		// We can't call eng.Run() since it loops — drive it directly.
		// Use a dedicated per-market channel approach instead.
	}

	// Alternative: use per-market replay feeds wired to state engines.
	// Rewind: use channels for proper async replay.
	// Reset: run state engines in goroutines with replay feeds.
	feedChans := make(map[string]chan feed.MarketEvent)
	for _, mktID := range marketIDs {
		ch := make(chan feed.MarketEvent, 64)
		feedChans[mktID] = ch

		eng := engines[mktID]
		wg.Add(1)
		go func(e *state.Engine, c <-chan feed.MarketEvent) {
			defer wg.Done()
			e.Run(backtestCtx, c)
		}(eng, ch)
	}

	// Send events into feed channels.
	for _, ev := range allEvents {
		if backtestCtx.Err() != nil {
			break
		}
		ch := feedChans[ev.MarketID]
		select {
		case ch <- ev:
		case <-backtestCtx.Done():
		}
	}

	// Close feed channels to signal end of data.
	for _, ch := range feedChans {
		close(ch)
	}

	// Give strategies time to process remaining signals.
	time.Sleep(100 * time.Millisecond)

	// Close all remaining positions.
	if err := sim.CloseAll(context.Background()); err != nil {
		r.log.Warn().Err(err).Msg("error closing positions at end of backtest")
	}

	// Shut down strategies and executor.
	cancel()
	wg.Wait()

	// Write report.
	return r.reporter.Write(sim, balance, start, end)
}

// collectMarketIDs returns deduplicated market IDs needed by the given strategies.
func (r *Runner) collectMarketIDs(strategies []strategy.Strategy) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, strat := range strategies {
		for _, mktID := range strat.Markets() {
			if !seen[mktID] {
				seen[mktID] = true
				ids = append(ids, mktID)
			}
		}
	}
	return ids
}

// mergeAndSort merges events from all markets sorted by timestamp.
func (r *Runner) mergeAndSort(marketEvents map[string][]feed.MarketEvent) []feed.MarketEvent {
	var all []feed.MarketEvent
	for _, evs := range marketEvents {
		all = append(all, evs...)
	}
	// Simple insertion sort is fine for moderate data; use sort.Slice for large sets.
	sortByTimestamp(all)
	return all
}

func sortByTimestamp(events []feed.MarketEvent) {
	n := len(events)
	for i := 1; i < n; i++ {
		key := events[i]
		j := i - 1
		for j >= 0 && events[j].Timestamp.After(key.Timestamp) {
			events[j+1] = events[j]
			j--
		}
		events[j+1] = key
	}
}

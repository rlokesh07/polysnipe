package discovery

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"polysnipe/internal/gamma"
)

// Strategy is the subset of the strategy interface the engine needs.
type Strategy interface {
	ID() string
	Tags() []string
}

// Engine polls the Gamma API and emits Subscribe/Unsubscribe commands.
type Engine struct {
	gamma        *gamma.Client
	watchlists   []Watchlist
	strategies   []strategyTagReader
	registry     map[string]bool // conditionID → currently subscribed
	commandCh    chan<- MarketCommand
	pollInterval time.Duration
	log          zerolog.Logger
}

// NewEngine creates a discovery engine.
func NewEngine(
	gc *gamma.Client,
	watchlists []Watchlist,
	strategies []Strategy,
	commandCh chan<- MarketCommand,
	pollInterval time.Duration,
	log zerolog.Logger,
) *Engine {
	readers := make([]strategyTagReader, len(strategies))
	for i, s := range strategies {
		readers[i] = s
	}
	return &Engine{
		gamma:        gc,
		watchlists:   watchlists,
		strategies:   readers,
		registry:     make(map[string]bool),
		commandCh:    commandCh,
		pollInterval: pollInterval,
		log:          log.With().Str("component", "discovery").Logger(),
	}
}

// Run starts the poll loop. It exits when ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.log.Info().Dur("poll_interval", e.pollInterval).Msg("discovery engine started")

	e.poll(ctx)

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.log.Info().Msg("discovery engine stopping")
			return
		case <-ticker.C:
			e.poll(ctx)
		}
	}
}

func (e *Engine) poll(ctx context.Context) {
	active := true
	closed := false

	// currentlyActive maps conditionID → GammaMarket (with event tags attached).
	currentlyActive := make(map[string]gamma.GammaMarket)

	// fetchAndFilter fetches events with the given opts and evaluates each market.
	fetchAndFilter := func(opts gamma.ListEventsOpts) bool {
		events, err := e.gamma.ListEvents(ctx, opts)
		if err != nil {
			e.log.Error().Err(err).Msg("gamma events poll failed; will retry next cycle")
			return false
		}
		for _, ev := range events {
			for _, mkt := range ev.Markets {
				if !mkt.Active || mkt.Closed {
					continue
				}
				for _, wl := range e.watchlists {
					if wl.matchesWatchlist(mkt) {
						currentlyActive[mkt.ConditionID] = mkt
						break
					}
				}
			}
		}
		return true
	}

	// Watchlists with no required tags need a full (unfiltered) fetch.
	// Watchlists with tags can be fetched more efficiently per tag.
	hasGlobalWatchlist := false
	for _, wl := range e.watchlists {
		if len(wl.Tags) == 0 {
			hasGlobalWatchlist = true
			break
		}
	}

	if hasGlobalWatchlist {
		opts := gamma.ListEventsOpts{Active: &active, Closed: &closed}
		if !fetchAndFilter(opts) {
			return
		}
	} else {
		for _, slug := range e.uniqueWatchlistTags() {
			tagSlug := slug
			opts := gamma.ListEventsOpts{Active: &active, Closed: &closed, TagSlug: &tagSlug}
			if !fetchAndFilter(opts) {
				return
			}
		}
	}

	e.log.Debug().Int("matching_markets", len(currentlyActive)).Msg("gamma poll completed")

	// Subscribe to newly discovered markets.
	for condID, gm := range currentlyActive {
		if e.registry[condID] {
			continue
		}

		matched := matchStrategies(gm, e.strategies)
		if len(matched) == 0 {
			continue
		}

		info := marketInfoFromGamma(gm)
		cmd := MarketCommand{
			Action:     Subscribe,
			Market:     info,
			Strategies: matched,
		}

		select {
		case e.commandCh <- cmd:
			e.registry[condID] = true
			e.log.Info().
				Str("event", "market_discovered").
				Str("market_id", condID).
				Str("question", gm.Question).
				Strs("matched_strategies", matched).
				Msg("market discovered; subscribing")
		case <-ctx.Done():
			return
		}
	}

	// Unsubscribe markets no longer in the active set.
	for condID := range e.registry {
		if _, ok := currentlyActive[condID]; ok {
			continue
		}

		cmd := MarketCommand{
			Action: Unsubscribe,
			Market: MarketInfo{ConditionID: condID},
		}

		select {
		case e.commandCh <- cmd:
			delete(e.registry, condID)
			e.log.Info().
				Str("event", "market_resolved").
				Str("market_id", condID).
				Msg("market no longer active; unsubscribing")
		case <-ctx.Done():
			return
		}
	}
}

// uniqueWatchlistTags returns deduplicated tag slugs across all watchlists.
func (e *Engine) uniqueWatchlistTags() []string {
	seen := make(map[string]bool)
	var out []string
	for _, wl := range e.watchlists {
		for _, tag := range wl.Tags {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	return out
}

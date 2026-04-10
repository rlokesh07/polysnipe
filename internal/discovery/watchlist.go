package discovery

import (
	"strings"
	"time"

	"polysnipe/internal/gamma"
)

// matchesWatchlist returns true if the market satisfies all of the watchlist's
// required tags AND all property filters.
func (w *Watchlist) matchesWatchlist(gm gamma.GammaMarket) bool {
	tagSet := marketTagSet(gm)

	// All watchlist tags must be present in the market's tag set.
	for _, required := range w.Tags {
		if !tagSet[required] {
			return false
		}
	}

	return w.Filters.matches(gm)
}

// matchesFilters evaluates a market against the property filters.
func (f *PropertyFilters) matches(gm gamma.GammaMarket) bool {
	if f.TitleContains != "" && !strings.Contains(strings.ToLower(gm.Question), strings.ToLower(f.TitleContains)) {
		return false
	}

	if f.Active && !gm.Active {
		return false
	}
	if gm.Closed {
		return false
	}

	if f.MinVolume24h != nil && gm.Volume24h < *f.MinVolume24h {
		return false
	}
	if f.MinLiquidity != nil && gm.Liquidity < *f.MinLiquidity {
		return false
	}

	if f.MaxExpiryMinutes != nil || f.MinExpiryMinutes != nil {
		if gm.EndDate.IsZero() {
			return false
		}
		remaining := time.Until(gm.EndDate)
		if remaining < 0 {
			// Market has already passed its end date; exclude it.
			return false
		}
		remainingMin := int(remaining.Minutes())

		if f.MaxExpiryMinutes != nil && remainingMin > *f.MaxExpiryMinutes {
			return false
		}
		if f.MinExpiryMinutes != nil && remainingMin < *f.MinExpiryMinutes {
			return false
		}
	}

	return true
}

// marketTagSet builds a set of tag slugs for quick lookup.
func marketTagSet(gm gamma.GammaMarket) map[string]bool {
	s := make(map[string]bool, len(gm.Tags))
	for _, t := range gm.Tags {
		s[t.Slug] = true
	}
	return s
}

// matchStrategies returns the IDs of strategies that have at least one tag
// overlapping with the market's tag set.
type strategyTagReader interface {
	ID() string
	Tags() []string
}

func matchStrategies(gm gamma.GammaMarket, strategies []strategyTagReader) []string {
	tagSet := marketTagSet(gm)
	var matched []string
	for _, s := range strategies {
		tags := s.Tags()
		if len(tags) == 0 {
			// No tags = match all markets.
			matched = append(matched, s.ID())
			continue
		}
		for _, t := range tags {
			if tagSet[t] {
				matched = append(matched, s.ID())
				break
			}
		}
	}
	return matched
}

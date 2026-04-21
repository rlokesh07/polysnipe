package discovery

import "polysnipe/internal/gamma"

// MarketAction indicates whether to subscribe or unsubscribe from a market.
type MarketAction int

const (
	Subscribe   MarketAction = iota
	Unsubscribe
)

// MarketInfo holds the data needed to set up a market goroutine stack.
type MarketInfo struct {
	ConditionID string
	TokenIDYes  string
	TokenIDNo   string
	Question    string
	Tags        []string
	EndDate     string // RFC3339
	NegRisk     bool
}

// MarketCommand is sent from the discovery engine to the orchestrator.
type MarketCommand struct {
	Action     MarketAction
	Market     MarketInfo
	Strategies []string // strategy IDs matched by tag intersection
}

// Watchlist defines a set of criteria for selecting markets.
type Watchlist struct {
	Name    string
	Tags    []string
	Filters PropertyFilters
}

// PropertyFilters restricts which markets a watchlist accepts.
type PropertyFilters struct {
	MaxExpiryMinutes *int
	MinExpiryMinutes *int
	OutcomeType      *string
	MinVolume24h     *float64
	MinLiquidity     *float64
	Active           bool
	TitleContains    string // if non-empty, market question must contain this substring (case-insensitive)
}

// marketInfoFromGamma converts a GammaMarket to MarketInfo.
func marketInfoFromGamma(gm gamma.GammaMarket) MarketInfo {
	tags := make([]string, 0, len(gm.Tags))
	for _, t := range gm.Tags {
		tags = append(tags, t.Slug)
	}

	var yes, no string
	if len(gm.TokenIDs) >= 2 {
		yes = gm.TokenIDs[0]
		no = gm.TokenIDs[1]
	} else if len(gm.TokenIDs) == 1 {
		yes = gm.TokenIDs[0]
	}

	return MarketInfo{
		ConditionID: gm.ConditionID,
		TokenIDYes:  yes,
		TokenIDNo:   no,
		Question:    gm.Question,
		Tags:        tags,
		EndDate:     gm.EndDateStr,
		NegRisk:     gm.NegRisk,
	}
}

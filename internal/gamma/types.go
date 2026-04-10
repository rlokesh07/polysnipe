package gamma

import (
	"encoding/json"
	"time"
)

// GammaMarket represents a market returned by the Gamma API.
type GammaMarket struct {
	ConditionID   string    `json:"conditionId"`
	QuestionID    string    `json:"questionId"`
	Slug          string    `json:"slug"`
	Question      string    `json:"question"`
	Description   string    `json:"description"`
	TokenIDs      []string  `json:"-"`
	OutcomePrices []string  `json:"-"`
	Volume24h     float64   `json:"volume24hrClob"`
	Liquidity     float64   `json:"liquidityClob"`
	Active        bool      `json:"active"`
	Closed        bool      `json:"closed"`
	EndDateStr    string    `json:"endDate"`
	EndDate       time.Time `json:"-"`
	EventID       string    `json:"eventId"`

	// Tags are inherited from the parent event, not present on the market itself.
	Tags []GammaTag `json:"-"`

	// raw string-encoded arrays from the wire
	clobTokenIDsRaw string
	outcomePricesRaw string
}

// gammaMarketWire is the JSON wire format.
// clobTokenIds and outcomePrices arrive as JSON-encoded strings.
type gammaMarketWire struct {
	ConditionID      string  `json:"conditionId"`
	QuestionID       string  `json:"questionId"`
	Slug             string  `json:"slug"`
	Question         string  `json:"question"`
	Description      string  `json:"description"`
	ClobTokenIDsRaw  string  `json:"clobTokenIds"`
	OutcomePricesRaw string  `json:"outcomePrices"`
	Volume24h        float64 `json:"volume24hrClob"`
	Liquidity        float64 `json:"liquidityClob"`
	Active           bool    `json:"active"`
	Closed           bool    `json:"closed"`
	EndDateStr       string  `json:"endDate"`
	EventID          string  `json:"eventId"`
}

// UnmarshalJSON handles clobTokenIds and outcomePrices being JSON-encoded strings.
func (m *GammaMarket) UnmarshalJSON(data []byte) error {
	var w gammaMarketWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.ConditionID = w.ConditionID
	m.QuestionID = w.QuestionID
	m.Slug = w.Slug
	m.Question = w.Question
	m.Description = w.Description
	m.Volume24h = w.Volume24h
	m.Liquidity = w.Liquidity
	m.Active = w.Active
	m.Closed = w.Closed
	m.EndDateStr = w.EndDateStr
	m.EventID = w.EventID

	if w.ClobTokenIDsRaw != "" {
		_ = json.Unmarshal([]byte(w.ClobTokenIDsRaw), &m.TokenIDs)
	}
	if w.OutcomePricesRaw != "" {
		_ = json.Unmarshal([]byte(w.OutcomePricesRaw), &m.OutcomePrices)
	}
	return nil
}

// GammaTag is a tag associated with an event.
type GammaTag struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"label"` // API field is "label"
}

// GammaEvent represents an event (group of related markets).
type GammaEvent struct {
	ID          string        `json:"id"`
	Slug        string        `json:"slug"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Active      bool          `json:"active"`
	Closed      bool          `json:"closed"`
	EndDateStr  string        `json:"endDate"`
	EndDate     time.Time     `json:"-"`
	Markets     []GammaMarket `json:"markets"`
	Tags        []GammaTag    `json:"tags"`
}

// ListMarketsOpts are query parameters for the ListMarkets call.
type ListMarketsOpts struct {
	Active  *bool
	Closed  *bool
	TagSlug *string
	Limit   int
	Offset  int
	Order   string
}

// ListEventsOpts are query parameters for the ListEvents call.
type ListEventsOpts struct {
	Active  *bool
	Closed  *bool
	TagSlug *string
	Limit   int
	Offset  int
}

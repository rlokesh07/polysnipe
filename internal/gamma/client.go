package gamma

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client wraps Polymarket's Gamma API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	throttle   <-chan time.Time // rate limiter ticker
}

// NewClient creates a Gamma API client.
// ratePerSec controls how many requests per second are allowed (default 10).
func NewClient(baseURL string, ratePerSec int) *Client {
	if ratePerSec <= 0 {
		ratePerSec = 10
	}
	interval := time.Second / time.Duration(ratePerSec)
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		throttle:   time.Tick(interval), //nolint:staticcheck
	}
}

// ListMarkets returns all active markets matching opts (handles pagination internally).
func (c *Client) ListMarkets(ctx context.Context, opts ListMarketsOpts) ([]GammaMarket, error) {
	return c.fetchAllMarkets(ctx, opts)
}

// GetMarket returns a single market by condition ID.
func (c *Client) GetMarket(ctx context.Context, conditionID string) (*GammaMarket, error) {
	var m GammaMarket
	if err := c.get(ctx, "/markets/"+conditionID, nil, &m); err != nil {
		return nil, err
	}
	parseMarketDates(&m)
	return &m, nil
}

// ListEvents returns all events matching opts (handles pagination internally).
func (c *Client) ListEvents(ctx context.Context, opts ListEventsOpts) ([]GammaEvent, error) {
	return c.fetchAllEvents(ctx, opts)
}

// GetEventBySlug returns an event by its slug, or nil if not found.
func (c *Client) GetEventBySlug(ctx context.Context, slug string) (*GammaEvent, error) {
	params := url.Values{}
	params.Set("slug", slug)
	var events []GammaEvent
	if err := c.get(ctx, "/events", params, &events); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	ev := &events[0]
	parseEventDates(ev)
	for i := range ev.Markets {
		ev.Markets[i].Tags = ev.Tags
		parseMarketDates(&ev.Markets[i])
	}
	return ev, nil
}

// ListTags returns all available tags.
func (c *Client) ListTags(ctx context.Context) ([]GammaTag, error) {
	var tags []GammaTag
	if err := c.get(ctx, "/tags", nil, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// SearchMarkets searches markets by keyword.
func (c *Client) SearchMarkets(ctx context.Context, query string) ([]GammaMarket, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("type", "markets")
	var markets []GammaMarket
	if err := c.get(ctx, "/search", params, &markets); err != nil {
		return nil, err
	}
	for i := range markets {
		parseMarketDates(&markets[i])
	}
	return markets, nil
}

// --- internal ---

func (c *Client) listMarketsPage(ctx context.Context, opts ListMarketsOpts) ([]GammaMarket, error) {
	params := url.Values{}
	if opts.Active != nil {
		params.Set("active", strconv.FormatBool(*opts.Active))
	}
	if opts.Closed != nil {
		params.Set("closed", strconv.FormatBool(*opts.Closed))
	}
	if opts.TagSlug != nil {
		params.Set("tag_slug", *opts.TagSlug)
	}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		params.Set("offset", strconv.Itoa(opts.Offset))
	}
	if opts.Order != "" {
		params.Set("order", opts.Order)
	}

	var markets []GammaMarket
	if err := c.get(ctx, "/markets", params, &markets); err != nil {
		return nil, err
	}
	for i := range markets {
		parseMarketDates(&markets[i])
	}
	return markets, nil
}

func (c *Client) listEventsPage(ctx context.Context, opts ListEventsOpts) ([]GammaEvent, error) {
	params := url.Values{}
	if opts.Active != nil {
		params.Set("active", strconv.FormatBool(*opts.Active))
	}
	if opts.Closed != nil {
		params.Set("closed", strconv.FormatBool(*opts.Closed))
	}
	if opts.TagSlug != nil {
		params.Set("tag_slug", *opts.TagSlug)
	}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		params.Set("offset", strconv.Itoa(opts.Offset))
	}

	var events []GammaEvent
	if err := c.get(ctx, "/events", params, &events); err != nil {
		return nil, err
	}
	for i := range events {
		parseEventDates(&events[i])
		// Propagate event tags down to each market.
		for j := range events[i].Markets {
			events[i].Markets[j].Tags = events[i].Tags
			parseMarketDates(&events[i].Markets[j])
		}
	}
	return events, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out interface{}) error {
	// Rate limiting: wait for a token from the ticker.
	select {
	case <-c.throttle:
	case <-ctx.Done():
		return ctx.Err()
	}

	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gamma API %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse response from %s: %w", path, err)
	}
	return nil
}

func parseMarketDates(m *GammaMarket) {
	if m.EndDateStr != "" {
		if t, err := time.Parse(time.RFC3339, m.EndDateStr); err == nil {
			m.EndDate = t
		}
	}
}

func parseEventDates(ev *GammaEvent) {
	if ev.EndDateStr != "" {
		if t, err := time.Parse(time.RFC3339, ev.EndDateStr); err == nil {
			ev.EndDate = t
		}
	}
	for i := range ev.Markets {
		parseMarketDates(&ev.Markets[i])
	}
}

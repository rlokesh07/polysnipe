package backtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/feed"
)

const polymarketTimeseries = "https://clob.polymarket.com/prices-history"

// Fetcher downloads historical market data from Polymarket and caches it locally.
type Fetcher struct {
	cacheDir   string
	httpClient *http.Client
	log        zerolog.Logger
}

// NewFetcher creates a new historical data fetcher.
func NewFetcher(cacheDir string, log zerolog.Logger) *Fetcher {
	return &Fetcher{
		cacheDir:   cacheDir,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		log:        log.With().Str("component", "fetcher").Logger(),
	}
}

// Fetch returns historical MarketEvents for a market token between start and end.
// Data is cached to disk; subsequent calls for the same token/range return cached data.
func (f *Fetcher) Fetch(marketID, tokenID string, start, end time.Time) ([]feed.MarketEvent, error) {
	cacheFile := filepath.Join(f.cacheDir, fmt.Sprintf("%s_%d_%d.json", tokenID, start.Unix(), end.Unix()))

	if data, err := os.ReadFile(cacheFile); err == nil {
		f.log.Debug().Str("file", cacheFile).Msg("loading from cache")
		return parseCached(marketID, data)
	}

	if err := os.MkdirAll(f.cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	f.log.Info().Str("market", marketID).Str("token", tokenID).
		Str("start", start.Format(time.DateOnly)).
		Str("end", end.Format(time.DateOnly)).
		Msg("fetching historical data")

	events, raw, err := f.download(marketID, tokenID, start, end)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(cacheFile, raw, 0644); err != nil {
		f.log.Warn().Err(err).Str("file", cacheFile).Msg("failed to cache data")
	}

	return events, nil
}

// polymarketPricePoint is the raw JSON from Polymarket's prices-history endpoint.
type polymarketPricePoint struct {
	T int64   `json:"t"` // unix timestamp
	P float64 `json:"p"` // price
}

type polymarketHistoryResponse struct {
	History []polymarketPricePoint `json:"history"`
}

func (f *Fetcher) download(marketID, tokenID string, start, end time.Time) ([]feed.MarketEvent, []byte, error) {
	// Fidelity = 60 seconds (1-minute bars).
	url := fmt.Sprintf("%s?market=%s&startTs=%d&endTs=%d&fidelity=60",
		polymarketTimeseries, tokenID, start.Unix(), end.Unix())

	resp, err := f.httpClient.Get(url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	events, err := parseCached(marketID, body)
	if err != nil {
		return nil, nil, err
	}
	return events, body, nil
}

func parseCached(marketID string, data []byte) ([]feed.MarketEvent, error) {
	var hr polymarketHistoryResponse
	if err := json.Unmarshal(data, &hr); err != nil {
		return nil, fmt.Errorf("parse history: %w", err)
	}

	events := make([]feed.MarketEvent, 0, len(hr.History))
	for i, pt := range hr.History {
		price := decimal.NewFromFloat(pt.P)
		ev := feed.MarketEvent{
			MarketID:  marketID,
			Timestamp: time.Unix(pt.T, 0).UTC(),
			EventType: "tick",
			LastPrice: price,
		}

		// Synthesize a simple order book from tick data.
		spread := decimal.NewFromFloat(0.01)
		bid := price.Sub(spread.Div(decimal.NewFromInt(2)))
		ask := price.Add(spread.Div(decimal.NewFromInt(2)))
		if bid.IsNegative() {
			bid = decimal.NewFromFloat(0.01)
		}
		if ask.GreaterThan(decimal.NewFromInt(1)) {
			ask = decimal.NewFromFloat(0.99)
		}

		ev.BestBid = bid
		ev.BestAsk = ask
		ev.OrderBook = feed.OrderBookSnapshot{
			Bids: []feed.OrderBookLevel{{Price: bid, Size: decimal.NewFromInt(1000)}},
			Asks: []feed.OrderBookLevel{{Price: ask, Size: decimal.NewFromInt(1000)}},
		}

		// Compute volume as price change.
		if i > 0 {
			prev := decimal.NewFromFloat(hr.History[i-1].P)
			ev.Volume = price.Sub(prev).Abs()
		}

		events = append(events, ev)
	}
	return events, nil
}

// GenerateSampleEvents generates synthetic market events for testing when no API is available.
func GenerateSampleEvents(marketID string, start, end time.Time) []feed.MarketEvent {
	var events []feed.MarketEvent
	price := decimal.NewFromFloat(0.5)
	tick := decimal.NewFromFloat(0.005)
	spread := decimal.NewFromFloat(0.01)
	direction := 1

	for t := start; t.Before(end); t = t.Add(time.Minute) {
		// Simple random walk simulation.
		if t.Minute()%7 == 0 {
			direction = -direction
		}
		delta := tick.Mul(decimal.NewFromInt(int64(direction)))
		price = price.Add(delta)
		if price.GreaterThan(decimal.NewFromFloat(0.95)) {
			price = decimal.NewFromFloat(0.95)
			direction = -1
		}
		if price.LessThan(decimal.NewFromFloat(0.05)) {
			price = decimal.NewFromFloat(0.05)
			direction = 1
		}

		bid := price.Sub(spread.Div(decimal.NewFromInt(2)))
		ask := price.Add(spread.Div(decimal.NewFromInt(2)))

		events = append(events, feed.MarketEvent{
			MarketID:  marketID,
			Timestamp: t.UTC(),
			EventType: "tick",
			LastPrice: price,
			BestBid:   bid,
			BestAsk:   ask,
			Volume:    delta.Abs(),
			OrderBook: feed.OrderBookSnapshot{
				Bids: []feed.OrderBookLevel{{Price: bid, Size: decimal.NewFromInt(500)}},
				Asks: []feed.OrderBookLevel{{Price: ask, Size: decimal.NewFromInt(500)}},
			},
		})
	}
	return events
}

package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"
)

// ReconnectConfig holds reconnection parameters.
type ReconnectConfig struct {
	MaxRetries   int
	BackoffBaseMS int
	BackoffMaxMS  int
}

// WSFeed maintains a WebSocket connection to the Polymarket CLOB feed for a single market.
type WSFeed struct {
	wsURL     string
	marketID  string
	tokenIDs  []string // YES and NO token IDs to subscribe to
	reconnect ReconnectConfig
	outCh     chan<- MarketEvent
	log       zerolog.Logger

	mu   sync.Mutex
	conn *websocket.Conn
}

// NewWSFeed creates a new WebSocket feed for the given market.
func NewWSFeed(wsURL, marketID string, tokenIDs []string, reconnect ReconnectConfig, outCh chan<- MarketEvent, log zerolog.Logger) *WSFeed {
	return &WSFeed{
		wsURL:     wsURL,
		marketID:  marketID,
		tokenIDs:  tokenIDs,
		reconnect: reconnect,
		outCh:     outCh,
		log:       log.With().Str("market", marketID).Logger(),
	}
}

// Run starts the feed goroutine. It blocks until ctx is cancelled.
func (f *WSFeed) Run(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := f.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempt++
			if f.reconnect.MaxRetries > 0 && attempt > f.reconnect.MaxRetries {
				f.log.Error().Err(err).Msg("max reconnect retries exceeded; feed stopped")
				return
			}
			backoff := f.backoffDuration(attempt)
			f.log.Warn().Err(err).Dur("backoff", backoff).Int("attempt", attempt).Msg("websocket disconnected; reconnecting")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		} else {
			attempt = 0
		}
	}
}

func (f *WSFeed) backoffDuration(attempt int) time.Duration {
	base := float64(f.reconnect.BackoffBaseMS)
	max := float64(f.reconnect.BackoffMaxMS)
	d := base * math.Pow(2, float64(attempt-1))
	if d > max {
		d = max
	}
	return time.Duration(d) * time.Millisecond
}

func (f *WSFeed) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, f.wsURL, http.Header{})
	if err != nil {
		return fmt.Errorf("dial %s: %w", f.wsURL, err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.conn = nil
		f.mu.Unlock()
		conn.Close()
	}()

	f.log.Info().Msg("websocket connected")

	// Subscribe to market channels.
	subMsg := map[string]interface{}{
		"assets_ids": f.tokenIDs,
		"type":       "market",
	}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Read loop.
	for {
		if ctx.Err() != nil {
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return nil
		}

		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		events, err := f.parseMessage(msg)
		if err != nil {
			f.log.Warn().Err(err).Msg("failed to parse message")
			continue
		}

		for _, ev := range events {
			select {
			case f.outCh <- ev:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// Close closes the underlying WebSocket connection.
func (f *WSFeed) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		f.conn.Close()
	}
}

// wsMessage is the raw JSON from Polymarket WebSocket.
type wsMessage struct {
	EventType string       `json:"event_type"`
	AssetID   string       `json:"asset_id"`
	Price     string       `json:"price"`
	Side      string       `json:"side"`
	Size      string       `json:"size"`
	Timestamp string       `json:"timestamp"`
	Bids      [][2]string  `json:"bids"`
	Asks      [][2]string  `json:"asks"`
}

func (f *WSFeed) parseMessage(raw []byte) ([]MarketEvent, error) {
	// Polymarket sends either a single object or an array.
	var msgs []wsMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, err
		}
	} else {
		var m wsMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		msgs = []wsMessage{m}
	}

	var events []MarketEvent
	for _, m := range msgs {
		ev, err := f.convertMessage(m)
		if err != nil {
			f.log.Debug().Err(err).Msg("skipping unconvertible message")
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}

func (f *WSFeed) convertMessage(m wsMessage) (MarketEvent, error) {
	ts := time.Now()
	if m.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
			ts = t
		}
	}

	ev := MarketEvent{
		MarketID:  f.marketID,
		Timestamp: ts,
	}

	switch m.EventType {
	case "price_change", "tick":
		ev.EventType = "tick"
		if m.Price != "" {
			ev.LastPrice, _ = decimal.NewFromString(m.Price)
		}
	case "book", "book_update":
		ev.EventType = "book_update"
		var obs OrderBookSnapshot
		for _, b := range m.Bids {
			p, _ := decimal.NewFromString(b[0])
			s, _ := decimal.NewFromString(b[1])
			obs.Bids = append(obs.Bids, OrderBookLevel{Price: p, Size: s})
		}
		for _, a := range m.Asks {
			p, _ := decimal.NewFromString(a[0])
			s, _ := decimal.NewFromString(a[1])
			obs.Asks = append(obs.Asks, OrderBookLevel{Price: p, Size: s})
		}
		ev.OrderBook = obs
		ev.BestBid = obs.BestBid()
		ev.BestAsk = obs.BestAsk()
	case "last_trade_price", "trade":
		ev.EventType = "trade"
		if m.Price != "" {
			ev.LastPrice, _ = decimal.NewFromString(m.Price)
		}
		if m.Size != "" {
			ev.Volume, _ = decimal.NewFromString(m.Size)
		}
	default:
		return ev, fmt.Errorf("unknown event type: %s", m.EventType)
	}

	return ev, nil
}

package feed

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// subscription holds routing info for one market.
type subscription struct {
	marketID   string
	tokenIDYes string
	tokenIDNo  string
	outCh      chan<- MarketEvent
}

// subCmd is sent to the SharedFeed to add or remove a subscription.
type subCmd struct {
	add bool
	sub subscription
}

// SharedFeed maintains a single WebSocket connection to Polymarket and
// routes incoming messages to per-market channels by asset_id.
type SharedFeed struct {
	wsURL     string
	reconnect ReconnectConfig
	log       zerolog.Logger

	cmdCh chan subCmd

	mu      sync.RWMutex
	byToken map[string]*subscription // tokenID → sub (both YES and NO point here)
	byMkt   map[string]*subscription // marketID → sub
}

// NewSharedFeed creates a shared WebSocket feed.
func NewSharedFeed(wsURL string, reconnect ReconnectConfig, log zerolog.Logger) *SharedFeed {
	return &SharedFeed{
		wsURL:     wsURL,
		reconnect: reconnect,
		log:       log.With().Str("component", "shared_feed").Logger(),
		cmdCh:     make(chan subCmd, 64),
		byToken:   make(map[string]*subscription),
		byMkt:     make(map[string]*subscription),
	}
}

// Subscribe routes events for the given market to outCh.
// The subscription message is sent on the live connection.
func (sf *SharedFeed) Subscribe(marketID, tokenIDYes, tokenIDNo string, outCh chan<- MarketEvent) {
	sf.cmdCh <- subCmd{add: true, sub: subscription{
		marketID:   marketID,
		tokenIDYes: tokenIDYes,
		tokenIDNo:  tokenIDNo,
		outCh:      outCh,
	}}
}

// Unsubscribe stops routing events for the given market.
func (sf *SharedFeed) Unsubscribe(marketID string) {
	sf.cmdCh <- subCmd{add: false, sub: subscription{marketID: marketID}}
}

// Run starts the shared WebSocket goroutine. Blocks until ctx is cancelled.
func (sf *SharedFeed) Run(ctx context.Context) {
	sf.log.Info().Msg("shared feed starting")
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := sf.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempt++
			if sf.reconnect.MaxRetries > 0 && attempt > sf.reconnect.MaxRetries {
				sf.log.Error().Err(err).Msg("max reconnect retries exceeded; shared feed stopped")
				return
			}
			backoff := sf.backoff(attempt)
			sf.log.Warn().Err(err).Dur("backoff", backoff).Int("attempt", attempt).Msg("shared feed disconnected; reconnecting")
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

func (sf *SharedFeed) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, sf.wsURL, http.Header{})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer conn.Close()

	sf.log.Info().Msg("shared WebSocket connected")

	// Re-subscribe all currently active markets after reconnect with one message.
	sf.mu.RLock()
	allTokens := sf.allTokenIDs()
	sf.mu.RUnlock()
	if len(allTokens) > 0 {
		if err := sf.sendSubscribe(conn, allTokens...); err != nil {
			return fmt.Errorf("resubscribe: %w", err)
		}
	}

	// writeCh serializes writes to conn from the cmd handler goroutine.
	writeCh := make(chan interface{}, 32)
	writeErr := make(chan error, 1)

	// Reset read deadline whenever a pong is received.
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	go func() {
		for msg := range writeCh {
			if err := conn.WriteJSON(msg); err != nil {
				writeErr <- err
				return
			}
		}
	}()

	// Keepalive: send a ping every 30 seconds so the server doesn't drop us.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					writeErr <- fmt.Errorf("ping: %w", err)
					return
				}
			}
		}
	}()

	// Handle subscription commands in a separate goroutine.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd, ok := <-sf.cmdCh:
				if !ok {
					return
				}
				if cmd.add {
					s := cmd.sub
					sf.mu.Lock()
					sf.byToken[s.tokenIDYes] = &s
					sf.byToken[s.tokenIDNo] = &s
					sf.byMkt[s.marketID] = &s
					allTokens := sf.allTokenIDs()
					sf.mu.Unlock()
					writeCh <- map[string]interface{}{
						"assets_ids": allTokens,
						"type":       "market",
					}
				} else {
					sf.mu.Lock()
					if s, ok := sf.byMkt[cmd.sub.marketID]; ok {
						delete(sf.byToken, s.tokenIDYes)
						delete(sf.byToken, s.tokenIDNo)
						delete(sf.byMkt, s.marketID)
					}
					allTokens := sf.allTokenIDs()
					sf.mu.Unlock()
					if len(allTokens) > 0 {
						writeCh <- map[string]interface{}{
							"assets_ids": allTokens,
							"type":       "market",
						}
					}
				}
			}
		}
	}()

	// Read loop.
	for {
		if ctx.Err() != nil {
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return nil
		}

		select {
		case err := <-writeErr:
			return fmt.Errorf("write: %w", err)
		default:
		}

		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		events, err := parseSharedMessage(raw)
		if err != nil {
			// Non-JSON messages (e.g. plain-text server notices) are expected — ignore them.
			sf.log.Debug().Str("raw", string(raw)).Msg("skipping non-JSON message")
			continue
		}

		sf.mu.RLock()
		for _, ev := range events {
			// Route by asset_id (stored in ev.MarketID temporarily as token).
			tokenID := ev.MarketID
			s, ok := sf.byToken[tokenID]
			if !ok {
				continue
			}
			// Only forward YES token events. NO token prices mirror YES prices
			// (NO_bid ≈ 1 - YES_ask) so they carry no additional information,
			// and mixing them causes the state engine to alternate between
			// YES and NO prices producing a zigzag artefact.
			if tokenID == s.tokenIDNo {
				continue
			}
			ev.MarketID = s.marketID
			select {
			case s.outCh <- ev:
			default:
			}
		}
		sf.mu.RUnlock()
	}
}

// sendSubscribe sends a single subscription message covering all provided token IDs.
func (sf *SharedFeed) sendSubscribe(conn *websocket.Conn, tokenIDs ...string) error {
	return conn.WriteJSON(map[string]interface{}{
		"assets_ids": tokenIDs,
		"type":       "market",
	})
}

// allTokenIDs returns all currently tracked token IDs. Caller must hold sf.mu.
func (sf *SharedFeed) allTokenIDs() []string {
	ids := make([]string, 0, len(sf.byToken))
	for id := range sf.byToken {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (sf *SharedFeed) backoff(attempt int) time.Duration {
	base := float64(sf.reconnect.BackoffBaseMS)
	max := float64(sf.reconnect.BackoffMaxMS)
	d := base * math.Pow(2, float64(attempt-1))
	if d > max {
		d = max
	}
	return time.Duration(d) * time.Millisecond
}

// parseSharedMessage parses a raw WS message, setting MarketID to asset_id for routing.
func parseSharedMessage(raw []byte) ([]MarketEvent, error) {
	return ParseMessages(raw, "")
}

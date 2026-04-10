package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"polysnipe/internal/config"
	"polysnipe/internal/executor"
	"polysnipe/internal/risk"
)

// StateProvider is implemented by the orchestrator to give the dashboard live data.
type StateProvider interface {
	Positions() []executor.Position
	SessionPnL() float64
	Balance() float64
	RecentFills() []executor.Fill
	IsHalted() bool
	ResumeTrading()
	PauseStrategy(id string)
	ResumeStrategy(id string)
	Uptime() time.Duration
	GoroutineCount() int
	LastError() string
	ActiveMarkets() []MarketStackInfo
	IsDryRun() bool
}

// MarketStackInfo describes a live market stack for the dashboard.
type MarketStackInfo struct {
	MarketID     string
	Question     string
	Tags         []string
	Strategies   []string
	SubscribedAt time.Time
	LastPrice    float64
	BestBid      float64
	BestAsk      float64
}

// Server is the dashboard HTTP + WebSocket server.
type Server struct {
	cfg      config.DashboardConfig
	risk     *risk.Manager
	state    StateProvider
	log      zerolog.Logger
	server   *http.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	clients map[*websocket.Conn]bool
}

// NewServer creates a new dashboard server.
func NewServer(cfg config.DashboardConfig, riskMgr *risk.Manager, state StateProvider, log zerolog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		risk:   riskMgr,
		state:  state,
		log:    log.With().Str("component", "dashboard").Logger(),
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow configured origins.
				origin := r.Header.Get("Origin")
				for _, allowed := range cfg.CORSAllowedOrigins {
					if allowed == "*" || allowed == origin {
						return true
					}
				}
				return false
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/positions", s.handlePositions)
	mux.HandleFunc("/api/markets", s.handleMarkets)
	mux.HandleFunc("/api/halt", s.handleHalt)
	mux.HandleFunc("/api/resume", s.handleResume)
	mux.HandleFunc("/api/strategy/pause", s.handlePauseStrategy)
	mux.HandleFunc("/api/strategy/resume", s.handleResumeStrategy)
	mux.HandleFunc("/ws", s.handleWS)

	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler: s.authMiddleware(mux),
	}
	return s
}

// Run starts the HTTP server and WebSocket push loop.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info().Str("addr", s.server.Addr).Msg("dashboard server starting")

	// WebSocket push goroutine.
	go s.pushLoop(ctx)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(shutCtx)
	}()

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken != "" {
			token := r.Header.Get("Authorization")
			if token != "Bearer "+s.cfg.AuthToken {
				// Allow WebSocket and static content without auth for simplicity.
				if r.URL.Path != "/" && r.URL.Path != "/ws" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) pushLoop(ctx context.Context) {
	interval := time.Duration(s.cfg.WebSocketPushIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.broadcastStatus()
		}
	}
}

func (s *Server) broadcastStatus() {
	status := s.buildStatus()
	data, err := json.Marshal(status)
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.clients {
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			conn.Close()
			delete(s.clients, conn)
		}
	}
}

func (s *Server) buildStatus() map[string]interface{} {
	positions := s.state.Positions()
	posData := make([]map[string]interface{}, 0, len(positions))
	for _, p := range positions {
		posData = append(posData, map[string]interface{}{
			"strategy_id": p.StrategyID,
			"market_id":   p.MarketID,
			"side":        p.Side.String(),
			"entry_price": p.EntryPrice.String(),
			"size":        p.Size.String(),
			"status":      fmt.Sprintf("%d", p.Status),
		})
	}

	activeMarkets := s.state.ActiveMarkets()
	mktData := make([]map[string]interface{}, 0, len(activeMarkets))
	for _, m := range activeMarkets {
		mktData = append(mktData, map[string]interface{}{
			"market_id":     m.MarketID,
			"question":      m.Question,
			"tags":          m.Tags,
			"strategies":    m.Strategies,
			"subscribed_at": m.SubscribedAt.UTC(),
			"age_seconds":   time.Since(m.SubscribedAt).Seconds(),
			"last_price":    m.LastPrice,
			"best_bid":      m.BestBid,
			"best_ask":      m.BestAsk,
		})
	}

	fills := s.state.RecentFills()
	fillData := make([]map[string]interface{}, 0, len(fills))
	for _, f := range fills {
		question := f.Question
		if question == "" {
			question = f.MarketID
		}
		fillData = append(fillData, map[string]interface{}{
			"strategy_id": f.StrategyID,
			"market_id":   f.MarketID,
			"question":    question,
			"side":        f.Side.String(),
			"price":       f.Price.String(),
			"size":        f.Size.String(),
			"fee":         f.Fee.String(),
			"pnl":         f.PnL.String(),
			"timestamp":   f.Timestamp.UTC(),
		})
	}

	return map[string]interface{}{
		"timestamp":      time.Now().UTC(),
		"halted":         s.state.IsHalted(),
		"dry_run":        s.state.IsDryRun(),
		"uptime_seconds": s.state.Uptime().Seconds(),
		"goroutines":     s.state.GoroutineCount(),
		"balance":        s.state.Balance(),
		"session_pnl":    s.state.SessionPnL(),
		"total_exposure": s.risk.TotalExposure().String(),
		"positions":      posData,
		"last_error":     s.state.LastError(),
		"active_markets": mktData,
		"market_count":   len(activeMarkets),
		"fills":          fillData,
		"fill_count":     len(fills),
	}
}

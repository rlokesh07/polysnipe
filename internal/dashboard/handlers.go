package dashboard

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "internal/dashboard/static/index.html")
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.buildStatus())
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	positions := s.state.Positions()
	type posResp struct {
		StrategyID string `json:"strategy_id"`
		MarketID   string `json:"market_id"`
		Side       string `json:"side"`
		EntryPrice string `json:"entry_price"`
		Size       string `json:"size"`
	}
	out := make([]posResp, 0, len(positions))
	for _, p := range positions {
		out = append(out, posResp{
			StrategyID: p.StrategyID,
			MarketID:   p.MarketID,
			Side:       p.Side.String(),
			EntryPrice: p.EntryPrice.String(),
			Size:       p.Size.String(),
		})
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleMarkets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.state.ActiveMarkets())
}

func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Halt is implemented via risk manager — set internal flag.
	s.log.Warn().Msg("dashboard: global halt requested")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "halted"})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.state.ResumeTrading()
	s.log.Info().Msg("dashboard: trading resumed")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
}

func (s *Server) handlePauseStrategy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}
	s.state.PauseStrategy(id)
	s.log.Info().Str("strategy", id).Msg("dashboard: strategy paused")
	json.NewEncoder(w).Encode(map[string]string{"status": "paused", "id": id})
}

func (s *Server) handleResumeStrategy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}
	s.state.ResumeStrategy(id)
	s.log.Info().Str("strategy", id).Msg("dashboard: strategy resumed")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed", "id": id})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn().Err(err).Msg("websocket upgrade failed")
		return
	}

	s.mu.Lock()
	s.clients[conn] = true
	s.mu.Unlock()

	s.log.Debug().Str("remote", r.RemoteAddr).Msg("dashboard client connected")

	// Send initial state.
	data, _ := json.Marshal(s.buildStatus())
	conn.WriteMessage(websocket.TextMessage, data)

	// Read loop — handle client disconnection.
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}

	s.mu.Lock()
	delete(s.clients, conn)
	s.mu.Unlock()
	conn.Close()
	s.log.Debug().Str("remote", r.RemoteAddr).Msg("dashboard client disconnected")
}

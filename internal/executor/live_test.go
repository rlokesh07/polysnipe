package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
)

// newTestExecutor builds a LiveExecutor pointed at serverURL with no credentials or risk/sizer.
// Only safe for tests that exercise HTTP methods directly (not handleSignal).
func newTestExecutor(t *testing.T, serverURL string) *LiveExecutor {
	t.Helper()
	e, err := NewLiveExecutor(
		config.ExecutionConfig{MaxOrderSpreadCents: 5.0, DefaultLimitOffsetBPS: 10},
		config.ConnectionConfig{RESTBaseURL: serverURL},
		nil,  // riskMgr — not used by HTTP methods
		nil,  // sizer   — not used by HTTP methods
		decimal.NewFromFloat(100),
		nil, // getMarketState — set per-test when needed
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatalf("NewLiveExecutor: %v", err)
	}
	return e
}

// ---------- fetchBalance ----------

func TestFetchBalance_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/balance" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{"balance": "250.75"})
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	bal, err := e.fetchBalance(context.Background())
	if err != nil {
		t.Fatalf("fetchBalance: %v", err)
	}
	want := decimal.NewFromFloat(250.75)
	if !bal.Equal(want) {
		t.Errorf("balance = %s, want %s", bal, want)
	}
}

func TestFetchBalance_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	_, err := e.fetchBalance(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFetchBalance_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	_, err := e.fetchBalance(context.Background())
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// ---------- fetchOpenOrders ----------

func TestFetchOpenOrders_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "OPEN" {
			t.Errorf("missing status=OPEN query param, got %q", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode([]clobOpenOrder{})
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	orders, err := e.fetchOpenOrders(context.Background())
	if err != nil {
		t.Fatalf("fetchOpenOrders: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("len(orders) = %d, want 0", len(orders))
	}
}

func TestFetchOpenOrders_WithOrders(t *testing.T) {
	want := []clobOpenOrder{{ID: "order-1"}, {ID: "order-2"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	orders, err := e.fetchOpenOrders(context.Background())
	if err != nil {
		t.Fatalf("fetchOpenOrders: %v", err)
	}
	if len(orders) != 2 || orders[0].ID != "order-1" || orders[1].ID != "order-2" {
		t.Errorf("orders = %+v, want %+v", orders, want)
	}
}

func TestFetchOpenOrders_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	_, err := e.fetchOpenOrders(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- reconcileOpenOrders ----------

func TestReconcileOpenOrders_CancelsOrphans(t *testing.T) {
	cancelled := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/orders":
			json.NewEncoder(w).Encode([]clobOpenOrder{{ID: "stale-1"}, {ID: "stale-2"}})
		case r.Method == http.MethodDelete:
			id := r.URL.Path[len("/order/"):]
			cancelled[id] = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	e.reconcileOpenOrders(context.Background())

	if !cancelled["stale-1"] || !cancelled["stale-2"] {
		t.Errorf("not all orphans cancelled: %v", cancelled)
	}
}

func TestReconcileOpenOrders_NoOrphans(t *testing.T) {
	cancelCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			cancelCalled = true
		}
		json.NewEncoder(w).Encode([]clobOpenOrder{})
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	e.reconcileOpenOrders(context.Background())

	if cancelCalled {
		t.Error("cancelOrder called when there were no orphans")
	}
}

// ---------- submitOrder ----------

func TestSubmitOrder_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/order" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(clobOrderResponse{OrderID: "new-order-99", Status: "PENDING"})
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	id, err := e.submitOrder(context.Background(), "mkt-1", 0, decimal.NewFromFloat(0.52), decimal.NewFromFloat(10))
	if err != nil {
		t.Fatalf("submitOrder: %v", err)
	}
	if id != "new-order-99" {
		t.Errorf("orderID = %q, want %q", id, "new-order-99")
	}
}

func TestSubmitOrder_CLOBError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":      "invalid price",
			"error_code": "invalid_price",
		})
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	_, err := e.submitOrder(context.Background(), "mkt-1", 0, decimal.NewFromFloat(0.52), decimal.NewFromFloat(10))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSubmitOrder_InsufficientBalance_RefreshesBalance(t *testing.T) {
	balanceFetched := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/order":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":      "insufficient balance",
				"error_code": "insufficient_balance",
			})
		case "/balance":
			balanceFetched = true
			json.NewEncoder(w).Encode(map[string]string{"balance": "5.00"})
		}
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	e.submitOrder(context.Background(), "mkt-1", 0, decimal.NewFromFloat(0.52), decimal.NewFromFloat(10))

	if !balanceFetched {
		t.Error("insufficient_balance error should trigger a balance refresh")
	}
}

// ---------- cancelOrder ----------

func TestCancelOrder_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/order/abc-123" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	if err := e.cancelOrder(context.Background(), "abc-123"); err != nil {
		t.Fatalf("cancelOrder: %v", err)
	}
}

func TestCancelOrder_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := newTestExecutor(t, srv.URL)
	if err := e.cancelOrder(context.Background(), "xyz"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- checkOrderStatus ----------

func TestCheckOrderStatus(t *testing.T) {
	tests := []struct {
		name       string
		response   map[string]string
		wantState  OrderState
		wantFilled string
	}{
		{
			name:       "filled",
			response:   map[string]string{"status": "FILLED", "size_filled": "10.5"},
			wantState:  OrderFilled,
			wantFilled: "10.5",
		},
		{
			name:       "matched",
			response:   map[string]string{"status": "MATCHED", "size_matched": "7"},
			wantState:  OrderFilled,
			wantFilled: "7",
		},
		{
			name:      "cancelled",
			response:  map[string]string{"status": "CANCELLED"},
			wantState: OrderCancelled,
		},
		{
			name:      "canceled_alt_spelling",
			response:  map[string]string{"status": "CANCELED"},
			wantState: OrderCancelled,
		},
		{
			name:       "partially_matched",
			response:   map[string]string{"status": "PARTIALLY_MATCHED", "size_filled": "3"},
			wantState:  OrderPartialFill,
			wantFilled: "3",
		},
		{
			name:      "pending",
			response:  map[string]string{"status": "OPEN"},
			wantState: OrderPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(tt.response)
			}))
			defer srv.Close()

			e := newTestExecutor(t, srv.URL)
			filled, state, err := e.checkOrderStatus(context.Background(), "order-1")
			if err != nil {
				t.Fatalf("checkOrderStatus: %v", err)
			}
			if state != tt.wantState {
				t.Errorf("state = %v, want %v", state, tt.wantState)
			}
			if tt.wantFilled != "" {
				want, _ := decimal.NewFromString(tt.wantFilled)
				if !filled.Equal(want) {
					t.Errorf("filled = %s, want %s", filled, want)
				}
			}
		})
	}
}

// ---------- resolveOrderPrice ----------

func TestResolveOrderPrice(t *testing.T) {
	tests := []struct {
		name               string
		bid, ask           float64
		sigPrice           float64
		maxSpreadCents     float64
		wantErr            bool
		wantPrice          float64
	}{
		{
			name:           "uses mid-price when spread ok",
			bid:            0.48, ask: 0.52,
			maxSpreadCents: 5.0,
			wantPrice:      0.50,
		},
		{
			name:           "rejects order when spread too wide",
			bid:            0.44, ask: 0.56, // 12 cents
			maxSpreadCents: 5.0,
			wantErr:        true,
		},
		{
			name:           "spread check disabled when maxSpreadCents is zero",
			bid:            0.30, ask: 0.70, // 40 cents — huge but check is off
			maxSpreadCents: 0,
			wantPrice:      0.50,
		},
		{
			name:           "falls back to sigPrice when no market state",
			bid:            0, ask: 0, // zero = no state
			sigPrice:       0.63,
			maxSpreadCents: 5.0,
			wantPrice:      0.63,
		},
		{
			name:           "falls back to 0.5 when no state and no sigPrice",
			bid:            0, ask: 0,
			maxSpreadCents: 5.0,
			wantPrice:      0.50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := NewLiveExecutor(
				config.ExecutionConfig{MaxOrderSpreadCents: tt.maxSpreadCents},
				config.ConnectionConfig{},
				nil, nil, decimal.Zero,
				func(marketID string) (decimal.Decimal, decimal.Decimal, string, bool) {
					bid := decimal.NewFromFloat(tt.bid)
					ask := decimal.NewFromFloat(tt.ask)
					return bid, ask, "", bid.IsPositive() && ask.IsPositive()
				},
				zerolog.Nop(),
			)
			if err != nil {
				t.Fatalf("NewLiveExecutor: %v", err)
			}

			price, err := e.resolveOrderPrice("mkt-x", decimal.NewFromFloat(tt.sigPrice))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveOrderPrice: %v", err)
			}
			want := decimal.NewFromFloat(tt.wantPrice)
			if !price.Equal(want) {
				t.Errorf("price = %s, want %s", price, want)
			}
		})
	}
}

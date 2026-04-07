package executor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"polysnipe/internal/config"
	"polysnipe/internal/risk"
	"polysnipe/internal/sizing"
	"polysnipe/internal/strategy"
)

// LiveExecutor places real orders on the Polymarket CLOB.
type LiveExecutor struct {
	cfg         config.ExecutionConfig
	connCfg     config.ConnectionConfig
	risk        *risk.Manager
	sizer       sizing.Sizer
	ledger      *Ledger
	log         zerolog.Logger
	httpClient  *http.Client
	privateKey  *ecdsa.PrivateKey

	mu          sync.Mutex
	balance     decimal.Decimal
	feedbackChs map[string]chan strategy.PositionUpdate
}

// NewLiveExecutor creates a live executor.
func NewLiveExecutor(
	cfg config.ExecutionConfig,
	connCfg config.ConnectionConfig,
	riskMgr *risk.Manager,
	sizer sizing.Sizer,
	balance decimal.Decimal,
	log zerolog.Logger,
) (*LiveExecutor, error) {
	var privKey *ecdsa.PrivateKey
	if connCfg.APISecret != "" {
		var err error
		privKey, err = crypto.HexToECDSA(connCfg.APISecret)
		if err != nil {
			log.Warn().Err(err).Msg("failed to parse API secret as ECDSA key; signing disabled")
		}
	}

	return &LiveExecutor{
		cfg:        cfg,
		connCfg:    connCfg,
		risk:       riskMgr,
		sizer:      sizer,
		ledger:     NewLedger(),
		log:        log.With().Str("component", "live_executor").Logger(),
		httpClient: &http.Client{Timeout: time.Duration(connCfg.RequestTimeoutMS) * time.Millisecond},
		privateKey: privKey,
		balance:    balance,
	}, nil
}

// Run starts the executor goroutine.
func (e *LiveExecutor) Run(ctx context.Context, signalCh <-chan strategy.Signal, feedbackChs map[string]chan strategy.PositionUpdate) {
	e.mu.Lock()
	e.feedbackChs = feedbackChs
	e.mu.Unlock()

	e.log.Info().Msg("live executor started")
	cooldown := time.Duration(e.cfg.CooldownBetweenOrdersMS) * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-signalCh:
			if !ok {
				return
			}
			if err := e.handleSignal(ctx, sig); err != nil {
				e.log.Error().Err(err).
					Str("strategy", sig.StrategyID).
					Str("market", sig.MarketID).
					Str("direction", sig.Direction.String()).
					Msg("signal rejected")
			}
			if cooldown > 0 {
				select {
				case <-time.After(cooldown):
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (e *LiveExecutor) handleSignal(ctx context.Context, sig strategy.Signal) error {
	e.mu.Lock()
	balance := e.balance
	e.mu.Unlock()

	if sig.Direction == strategy.Close {
		if err := e.ledger.ValidateClose(sig.StrategyID, sig.MarketID); err != nil {
			return err
		}
		return e.placeCloseOrder(ctx, sig)
	}

	// Entry signal — check for existing position.
	if e.ledger.HasOpenPosition(sig.StrategyID, sig.MarketID) {
		return fmt.Errorf("strategy %s already has open position in %s", sig.StrategyID, sig.MarketID)
	}

	size := e.sizer.Size(balance, sig.StrategyID)
	if err := e.risk.Check(sig, sig.StrategyID, size); err != nil {
		return fmt.Errorf("risk check failed: %w", err)
	}

	return e.placeEntryOrder(ctx, sig, size)
}

func (e *LiveExecutor) placeEntryOrder(ctx context.Context, sig strategy.Signal, size decimal.Decimal) error {
	// Calculate limit price (mid ± offset).
	// In a real implementation we'd look up current mid price from the state engine.
	// For now, use a placeholder price of 0.5 with offset applied.
	offsetBPS := decimal.NewFromFloat(float64(e.cfg.DefaultLimitOffsetBPS) / 10000.0)
	midPrice := decimal.NewFromFloat(0.5)
	var limitPrice decimal.Decimal
	if sig.Direction == strategy.BuyYes {
		limitPrice = midPrice.Add(midPrice.Mul(offsetBPS))
	} else {
		limitPrice = midPrice.Sub(midPrice.Mul(offsetBPS))
	}

	orderID, err := e.submitOrder(ctx, sig.MarketID, sig.Direction, limitPrice, size)
	if err != nil {
		return fmt.Errorf("submit order: %w", err)
	}

	order := Order{
		ID:         orderID,
		StrategyID: sig.StrategyID,
		MarketID:   sig.MarketID,
		Side:       sig.Direction,
		Price:      limitPrice,
		Size:       size,
		State:      OrderPending,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	e.ledger.AddOrder(order)

	pos := Position{
		StrategyID:  sig.StrategyID,
		MarketID:    sig.MarketID,
		Side:        sig.Direction,
		EntryPrice:  limitPrice,
		Size:        size,
		OpenOrderID: orderID,
		Status:      strategy.StatusOpen,
	}
	if err := e.ledger.OpenPosition(pos); err != nil {
		return err
	}

	e.risk.RecordOpen(sig.StrategyID, sig.MarketID, size)
	e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
		StrategyID:  sig.StrategyID,
		MarketID:    sig.MarketID,
		Status:      strategy.StatusOpen,
		Side:        sig.Direction,
		EntryPrice:  limitPrice,
		Size:        size,
		OpenOrderID: orderID,
		OrderState:  strategy.OrderPending,
	})

	// Monitor the order asynchronously.
	go e.monitorOrder(ctx, order)
	return nil
}

func (e *LiveExecutor) placeCloseOrder(ctx context.Context, sig strategy.Signal) error {
	pos := e.ledger.GetPosition(sig.StrategyID, sig.MarketID)
	if pos == nil {
		return fmt.Errorf("no position to close")
	}

	// Close direction is opposite of entry.
	closeDir := strategy.BuyNo
	if pos.Side == strategy.BuyNo {
		closeDir = strategy.BuyYes
	}

	midPrice := decimal.NewFromFloat(0.5)
	orderID, err := e.submitOrder(ctx, sig.MarketID, closeDir, midPrice, pos.Size)
	if err != nil {
		return fmt.Errorf("submit close order: %w", err)
	}

	e.ledger.ClosePosition(sig.StrategyID, sig.MarketID)
	e.risk.RecordClose(sig.StrategyID, sig.MarketID, pos.Size, decimal.Zero)

	e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
		StrategyID:  sig.StrategyID,
		MarketID:    sig.MarketID,
		Status:      strategy.StatusClosed,
		Side:        pos.Side,
		EntryPrice:  pos.EntryPrice,
		Size:        pos.Size,
		OpenOrderID: orderID,
		OrderState:  strategy.OrderPending,
	})
	return nil
}

func (e *LiveExecutor) monitorOrder(ctx context.Context, order Order) {
	timeout := time.Duration(e.cfg.OrderTimeoutSeconds) * time.Second
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			// Timeout — cancel the order.
			e.log.Warn().Str("order_id", order.ID).Msg("order timed out; cancelling")
			if err := e.cancelOrder(ctx, order.ID); err != nil {
				e.log.Error().Err(err).Str("order_id", order.ID).Msg("failed to cancel order")
			}
			e.ledger.ClosePosition(order.StrategyID, order.MarketID)
			e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
				StrategyID:  order.StrategyID,
				MarketID:    order.MarketID,
				Status:      strategy.StatusClosed,
				Side:        order.Side,
				OpenOrderID: order.ID,
				OrderState:  strategy.OrderExpired,
			})
			return
		case <-ticker.C:
			filled, state, err := e.checkOrderStatus(ctx, order.ID)
			if err != nil {
				e.log.Warn().Err(err).Str("order_id", order.ID).Msg("order status check failed")
				continue
			}
			order.FilledSize = filled
			order.State = state
			e.ledger.UpdateOrder(order)

			if state == OrderFilled {
				e.log.Info().Str("order_id", order.ID).Msg("order filled")
				e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
					StrategyID:  order.StrategyID,
					MarketID:    order.MarketID,
					Status:      strategy.StatusOpen,
					Side:        order.Side,
					EntryPrice:  order.Price,
					Size:        order.FilledSize,
					OpenOrderID: order.ID,
					OrderState:  strategy.OrderFilled,
				})
				return
			}
			if state == OrderCancelled {
				e.log.Info().Str("order_id", order.ID).Msg("order cancelled")
				e.ledger.ClosePosition(order.StrategyID, order.MarketID)
				e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
					StrategyID:  order.StrategyID,
					MarketID:    order.MarketID,
					Status:      strategy.StatusClosed,
					Side:        order.Side,
					OpenOrderID: order.ID,
					OrderState:  strategy.OrderCancelled,
				})
				return
			}
		}
	}
}

// CancelAll cancels all open/pending orders.
func (e *LiveExecutor) CancelAll(ctx context.Context) error {
	positions := e.ledger.OpenPositions()
	for _, pos := range positions {
		if pos.OpenOrderID != "" {
			if err := e.cancelOrder(ctx, pos.OpenOrderID); err != nil {
				e.log.Error().Err(err).Str("order_id", pos.OpenOrderID).Msg("cancel failed")
			}
		}
	}
	return nil
}

// CloseAll closes all open positions.
func (e *LiveExecutor) CloseAll(ctx context.Context) error {
	positions := e.ledger.OpenPositions()
	for _, pos := range positions {
		sig := strategy.Signal{
			StrategyID: pos.StrategyID,
			MarketID:   pos.MarketID,
			Direction:  strategy.Close,
			Timestamp:  time.Now(),
		}
		if err := e.placeCloseOrder(ctx, sig); err != nil {
			e.log.Error().Err(err).
				Str("strategy", pos.StrategyID).
				Str("market", pos.MarketID).
				Msg("close position failed during shutdown")
		}
	}
	return nil
}

// Positions returns all tracked positions.
func (e *LiveExecutor) Positions() []Position {
	return e.ledger.AllPositions()
}

func (e *LiveExecutor) sendFeedback(strategyID string, update strategy.PositionUpdate) {
	e.mu.Lock()
	ch, ok := e.feedbackChs[strategyID]
	e.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- update:
	default:
		// drain and replace
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- update:
		default:
		}
	}
}

// --- CLOB API client methods ---

type clobOrderRequest struct {
	OrderType   string `json:"orderType"`
	TokenID     string `json:"tokenId"`
	Side        string `json:"side"`
	Price       string `json:"price"`
	Size        string `json:"size"`
	TimeInForce string `json:"timeInForce"`
	Nonce       int64  `json:"nonce"`
	MakerAmount string `json:"makerAmount"`
	TakerAmount string `json:"takerAmount"`
	Expiration  int64  `json:"expiration"`
	SignedOrder string `json:"signature"`
}

type clobOrderResponse struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`
}

func (e *LiveExecutor) submitOrder(ctx context.Context, marketID string, dir strategy.Direction, price, size decimal.Decimal) (string, error) {
	side := "BUY"
	if dir == strategy.BuyNo {
		side = "SELL"
	}

	payload := map[string]interface{}{
		"orderType":   "GTC",
		"tokenId":     marketID,
		"side":        side,
		"price":       price.String(),
		"size":        size.String(),
		"timeInForce": "GTC",
	}

	if e.privateKey != nil {
		sig, err := e.signPayload(payload)
		if err != nil {
			e.log.Warn().Err(err).Msg("EIP-712 signing failed; sending unsigned")
		} else {
			payload["signature"] = sig
		}
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.connCfg.RESTBaseURL+"/order", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.connCfg.APIKey != "" {
		req.Header.Set("POLY_ADDRESS", e.connCfg.APIKey)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP post /order: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("CLOB API error %d: %s", resp.StatusCode, string(respBody))
	}

	var orderResp clobOrderResponse
	if err := json.Unmarshal(respBody, &orderResp); err != nil {
		return "", fmt.Errorf("parse order response: %w", err)
	}
	return orderResp.OrderID, nil
}

func (e *LiveExecutor) cancelOrder(ctx context.Context, orderID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, e.connCfg.RESTBaseURL+"/order/"+orderID, nil)
	if err != nil {
		return err
	}
	if e.connCfg.APIKey != "" {
		req.Header.Set("POLY_ADDRESS", e.connCfg.APIKey)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel order %s: status %d", orderID, resp.StatusCode)
	}
	return nil
}

func (e *LiveExecutor) checkOrderStatus(ctx context.Context, orderID string) (decimal.Decimal, OrderState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.connCfg.RESTBaseURL+"/order/"+orderID, nil)
	if err != nil {
		return decimal.Zero, OrderPending, err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return decimal.Zero, OrderPending, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Status       string `json:"status"`
		SizeFilled   string `json:"size_filled"`
		SizeMatched  string `json:"size_matched"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return decimal.Zero, OrderPending, err
	}

	filled := decimal.Zero
	if result.SizeFilled != "" {
		filled, _ = decimal.NewFromString(result.SizeFilled)
	} else if result.SizeMatched != "" {
		filled, _ = decimal.NewFromString(result.SizeMatched)
	}

	var state OrderState
	switch result.Status {
	case "MATCHED", "FILLED":
		state = OrderFilled
	case "CANCELLED", "CANCELED":
		state = OrderCancelled
	case "PARTIALLY_MATCHED", "PARTIAL":
		state = OrderPartialFill
	default:
		state = OrderPending
	}
	return filled, state, nil
}

// signPayload signs a JSON payload using EIP-712.
func (e *LiveExecutor) signPayload(payload map[string]interface{}) (string, error) {
	if e.privateKey == nil {
		return "", fmt.Errorf("no private key configured")
	}

	// Build a minimal EIP-712 typed data structure for Polymarket CLOB orders.
	typedData := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
			},
			"Order": []apitypes.Type{
				{Name: "orderType", Type: "string"},
				{Name: "tokenId", Type: "string"},
				{Name: "side", Type: "string"},
				{Name: "price", Type: "string"},
				{Name: "size", Type: "string"},
			},
		},
		PrimaryType: "Order",
		Domain: apitypes.TypedDataDomain{
			Name:    "Polymarket CLOB",
			Version: "1",
			ChainId: math.NewHexOrDecimal256(137), // Polygon
		},
		Message: apitypes.TypedDataMessage{},
	}
	for k, v := range payload {
		typedData.Message[k] = v
	}

	hash, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return "", fmt.Errorf("EIP-712 hash: %w", err)
	}

	sig, err := crypto.Sign(accounts.TextHash(hash), e.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	// Adjust v for EIP-712 (v = 27 or 28)
	sig[64] += 27

	return fmt.Sprintf("0x%x", sig), nil
}

package executor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gmath "github.com/ethereum/go-ethereum/common/math"
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
	cfg            config.ExecutionConfig
	connCfg        config.ConnectionConfig
	risk           *risk.Manager
	sizer          sizing.Sizer
	ledger         *Ledger
	log            zerolog.Logger
	httpClient     *http.Client
	privateKey     *ecdsa.PrivateKey // Phantom EOA key — EIP-712 signer
	walletAddress  string            // EOA address derived from privateKey — order signer
	funderAddress  string            // Gnosis Safe proxy from config — order maker + balance owner
	getMarketState func(marketID string) (bid, ask decimal.Decimal, tokenIDYes, tokenIDNo string, ok bool)
	isNegRisk      func(marketID string) bool
	negRiskCache   sync.Map // tokenID string → bool
	feeRateCache   sync.Map // tokenID string → int

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
	getMarketState func(marketID string) (bid, ask decimal.Decimal, tokenIDYes, tokenIDNo string, ok bool),
	isNegRisk func(marketID string) bool,
	log zerolog.Logger,
) (*LiveExecutor, error) {
	var privKey *ecdsa.PrivateKey
	var walletAddress string

	keyHex := strings.TrimPrefix(connCfg.WalletPrivateKey, "0x")
	if keyHex == "" {
		// Backward compat: fall back to APISecret if WalletPrivateKey not set.
		keyHex = strings.TrimPrefix(connCfg.APISecret, "0x")
	}
	if keyHex != "" {
		var err error
		privKey, err = crypto.HexToECDSA(keyHex)
		if err != nil {
			log.Warn().Err(err).Msg("failed to parse wallet private key; order signing disabled")
		} else {
			pubKey := privKey.Public().(*ecdsa.PublicKey)
			walletAddress = crypto.PubkeyToAddress(*pubKey).Hex()
			log.Info().Str("wallet_address", walletAddress).Msg("wallet loaded")
		}
	}

	return &LiveExecutor{
		cfg:            cfg,
		connCfg:        connCfg,
		risk:           riskMgr,
		sizer:          sizer,
		ledger:         NewLedger(),
		log:            log.With().Str("component", "live_executor").Logger(),
		httpClient:     &http.Client{Timeout: time.Duration(connCfg.RequestTimeoutMS) * time.Millisecond},
		privateKey:     privKey,
		walletAddress:  walletAddress,
		funderAddress:  connCfg.FunderAddress,
		balance:        balance,
		getMarketState: getMarketState,
		isNegRisk:      isNegRisk,
	}, nil
}

// resolveOrderPrice returns the mid-price to use for a limit order.
// When checkSpread is true, returns an error if the spread exceeds MaxOrderSpreadCents.
// Falls back to sig.Price, then 0.5 with a warning.
func (e *LiveExecutor) resolveOrderPrice(marketID string, sigPrice decimal.Decimal, checkSpread bool) (decimal.Decimal, error) {
	if e.getMarketState != nil {
		bid, ask, _, _, ok := e.getMarketState(marketID)
		if ok && bid.IsPositive() && ask.IsPositive() {
			if checkSpread && e.cfg.MaxOrderSpreadCents > 0 {
				spread, _ := ask.Sub(bid).Mul(decimal.NewFromInt(100)).Float64()
				if spread > e.cfg.MaxOrderSpreadCents {
					return decimal.Zero, fmt.Errorf("spread %.2f¢ > max %.2f¢ for %s; skipping",
						spread, e.cfg.MaxOrderSpreadCents, marketID)
				}
			}
			return bid.Add(ask).Div(decimal.NewFromInt(2)), nil
		}
	}
	if sigPrice.IsPositive() {
		return sigPrice, nil
	}
	e.log.Warn().Str("market", marketID).Msg("no market state available; pricing at 0.5")
	return decimal.NewFromFloat(0.5), nil
}

// Balance returns the current tracked USDC balance.
func (e *LiveExecutor) Balance() decimal.Decimal {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.balance
}

// fetchBalance retrieves the current USDC balance from the CLOB REST API.
func (e *LiveExecutor) fetchBalance(ctx context.Context) (decimal.Decimal, error) {
	const balPath = "/balance-allowance"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.connCfg.RESTBaseURL+balPath+"?asset_type=COLLATERAL&signature_type=2", nil)
	if err != nil {
		return decimal.Zero, err
	}
	for k, v := range e.buildL2Headers("GET", balPath, "") {
		req.Header.Set(k, v)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("GET /balance-allowance: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, fmt.Errorf("GET /balance-allowance status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return decimal.Zero, fmt.Errorf("parse balance: %w", err)
	}
	raw, err := decimal.NewFromString(result.Balance)
	if err != nil {
		return decimal.Zero, err
	}
	return raw.Div(decimal.NewFromInt(1_000_000)), nil
}

// resolveNegRisk returns whether a token ID is a neg-risk market, querying the CLOB if not cached.
func (e *LiveExecutor) resolveNegRisk(ctx context.Context, tokenID string) bool {
	if v, ok := e.negRiskCache.Load(tokenID); ok {
		return v.(bool)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.connCfg.RESTBaseURL+"/neg-risk?token_id="+tokenID, nil)
	if err != nil {
		return false
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		NegRisk bool `json:"neg_risk"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	e.negRiskCache.Store(tokenID, result.NegRisk)
	return result.NegRisk
}

// resolveFeeRate returns the base fee rate in bps for a token, querying the CLOB if not cached.
func (e *LiveExecutor) resolveFeeRate(ctx context.Context, tokenID string) int {
	if v, ok := e.feeRateCache.Load(tokenID); ok {
		return v.(int)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.connCfg.RESTBaseURL+"/fee-rate?token_id="+tokenID, nil)
	if err != nil {
		return 0
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		BaseFee int `json:"base_fee"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0
	}
	e.feeRateCache.Store(tokenID, result.BaseFee)
	return result.BaseFee
}

// clobOpenOrder represents an open order from the CLOB.
type clobOpenOrder struct {
	ID string `json:"id"`
}

// fetchOpenOrders returns all open orders for the current account.
func (e *LiveExecutor) fetchOpenOrders(ctx context.Context) ([]clobOpenOrder, error) {
	const path = "/data/orders"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.connCfg.RESTBaseURL+path+"?maker="+e.funderAddress+"&status=OPEN", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range e.buildL2Headers("GET", path, "") {
		req.Header.Set(k, v)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /data/orders: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /data/orders status %d: %s", resp.StatusCode, body)
	}
	var orders []clobOpenOrder
	return orders, json.Unmarshal(body, &orders)
}

// reconcileOpenOrders cancels any orders left open from a prior session.
func (e *LiveExecutor) reconcileOpenOrders(ctx context.Context) {
	orders, err := e.fetchOpenOrders(ctx)
	if err != nil {
		e.log.Error().Err(err).Msg("startup reconciliation failed; orphan orders may exist")
		return
	}
	for _, o := range orders {
		e.log.Warn().Str("order_id", o.ID).Msg("cancelling orphan order from prior session")
		if err := e.cancelOrder(ctx, o.ID); err != nil {
			e.log.Error().Err(err).Str("order_id", o.ID).Msg("failed to cancel orphan")
		}
	}
	e.log.Info().Int("cancelled", len(orders)).Msg("startup reconciliation complete")
}

// Run starts the executor goroutine.
func (e *LiveExecutor) Run(ctx context.Context, signalCh <-chan strategy.Signal, feedbackChs map[string]chan strategy.PositionUpdate) {
	e.mu.Lock()
	e.feedbackChs = feedbackChs
	e.mu.Unlock()

	// Fetch real balance at startup.
	if bal, err := e.fetchBalance(ctx); err != nil {
		e.log.Error().Err(err).Msg("initial balance fetch failed; using config value")
	} else {
		e.mu.Lock()
		e.balance = bal
		e.mu.Unlock()
		e.log.Info().Str("balance", bal.String()).Msg("USDC balance fetched from CLOB")
	}

	// Cancel any open orders left from a prior session.
	reconCtx, reconCancel := context.WithTimeout(ctx, 15*time.Second)
	e.reconcileOpenOrders(reconCtx)
	reconCancel()

	// Periodic balance refresh to correct drift from fee estimation.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if bal, err := e.fetchBalance(ctx); err == nil {
					e.mu.Lock()
					e.balance = bal
					e.mu.Unlock()
				}
			}
		}
	}()

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
			// Ledger has no record of this position — strategy is out of sync.
			// Send StatusClosed so the strategy resets and stops trying to close.
			e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
				StrategyID: sig.StrategyID,
				MarketID:   sig.MarketID,
				Status:     strategy.StatusClosed,
			})
			return err
		}
		if err := e.placeCloseOrder(ctx, sig); err != nil {
			// Close attempt failed (e.g. spread too wide, transient error).
			// Tell the strategy the position is still open so it can retry.
			update := strategy.PositionUpdate{
				StrategyID: sig.StrategyID,
				MarketID:   sig.MarketID,
				Status:     strategy.StatusOpen,
			}
			if pos := e.ledger.GetPosition(sig.StrategyID, sig.MarketID); pos != nil {
				update.Side = pos.Side
				update.EntryPrice = pos.EntryPrice
				update.Size = pos.Size
			}
			e.sendFeedback(sig.StrategyID, update)
			return err
		}
		return nil
	}

	// Entry signal — check for existing position.
	if e.ledger.HasOpenPosition(sig.StrategyID, sig.MarketID) {
		return fmt.Errorf("strategy %s already has open position in %s", sig.StrategyID, sig.MarketID)
	}

	size := e.sizer.Size(balance, sig.StrategyID)

	if err := e.risk.Check(sig, sig.StrategyID, size); err != nil {
		return fmt.Errorf("risk check failed: %w", err)
	}

	if err := e.placeEntryOrder(ctx, sig, size); err != nil {
		// Notify the strategy so it resets hasPos and can re-enter later.
		e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
			StrategyID: sig.StrategyID,
			MarketID:   sig.MarketID,
			Status:     strategy.StatusNone,
		})
		return err
	}
	return nil
}

func (e *LiveExecutor) placeEntryOrder(ctx context.Context, sig strategy.Signal, size decimal.Decimal) error {
	midPrice, err := e.resolveOrderPrice(sig.MarketID, sig.Price, true)
	if err != nil {
		return err
	}
	offsetBPS := decimal.NewFromFloat(float64(e.cfg.DefaultLimitOffsetBPS) / 10000.0)
	var limitPrice decimal.Decimal
	if sig.Direction == strategy.BuyYes {
		limitPrice = midPrice.Add(midPrice.Mul(offsetBPS))
	} else {
		limitPrice = midPrice.Sub(midPrice.Mul(offsetBPS))
	}

	// Cost in USDC = price_per_contract × contracts.
	cost := limitPrice.Mul(size)
	e.mu.Lock()
	balance := e.balance
	e.mu.Unlock()
	if cost.GreaterThan(balance) {
		return fmt.Errorf("insufficient balance %.4f USDC (order costs %.4f); skipping",
			balance.InexactFloat64(), cost.InexactFloat64())
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

	// Do NOT send StatusOpen feedback here — the order is merely pending on the CLOB,
	// not filled. monitorOrder will send StatusOpen once the order actually fills,
	// or StatusNone/StatusClosed if it times out or is cancelled. Sending StatusOpen
	// on submit was the root cause of 500+ phantom-position errors.
	go e.monitorOrder(ctx, order)
	return nil
}

func (e *LiveExecutor) placeCloseOrder(ctx context.Context, sig strategy.Signal) error {
	pos := e.ledger.GetPosition(sig.StrategyID, sig.MarketID)
	if pos == nil {
		return fmt.Errorf("no position to close")
	}

	// Exit sells back the same token that was bought at entry.
	closeDir := strategy.SellYes
	if pos.Side == strategy.BuyNo {
		closeDir = strategy.SellNo
	}

	midPrice, err := e.resolveOrderPrice(sig.MarketID, sig.Price, false)
	if err != nil {
		return err
	}
	// Selling: offer slightly below mid to increase fill probability.
	offsetBPS := decimal.NewFromFloat(float64(e.cfg.DefaultLimitOffsetBPS) / 10000.0)
	closePrice := midPrice.Sub(midPrice.Mul(offsetBPS))

	e.log.Debug().
		Str("market_id", sig.MarketID).
		Str("strategy_id", sig.StrategyID).
		Str("side", string(closeDir)).
		Str("size", pos.Size.String()).
		Str("entry_price", pos.EntryPrice.String()).
		Str("close_price", closePrice.String()).
		Str("mid", midPrice.String()).
		Msg("placing close order")

	orderID, err := e.submitOrder(ctx, sig.MarketID, closeDir, closePrice, pos.Size)
	if err != nil {
		// "orderbook does not exist" means the market has already resolved.
		// Force-close the position so the strategy stops retrying.
		if strings.Contains(err.Error(), "orderbook") {
			e.ledger.ClosePosition(sig.StrategyID, sig.MarketID)
			e.risk.RecordClose(sig.StrategyID, sig.MarketID, pos.Size, decimal.Zero)
			e.sendFeedback(sig.StrategyID, strategy.PositionUpdate{
				StrategyID: sig.StrategyID,
				MarketID:   sig.MarketID,
				Status:     strategy.StatusClosed,
				Side:       pos.Side,
				EntryPrice: pos.EntryPrice,
				Size:       pos.Size,
			})
			e.log.Warn().
				Str("market_id", sig.MarketID).
				Str("strategy_id", sig.StrategyID).
				Msg("market resolved; force-closing position without exit order")
			return nil
		}
		return fmt.Errorf("submit close order: %w", err)
	}

	// Keep the position in the ledger until fill is confirmed.
	// monitorOrder will close it on fill and send StatusClosed feedback.
	closeOrder := Order{
		ID:         orderID,
		StrategyID: sig.StrategyID,
		MarketID:   sig.MarketID,
		Side:       closeDir,
		IsClose:    true,
		Price:      closePrice,
		Size:       pos.Size,
		State:      OrderPending,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	e.ledger.AddOrder(closeOrder)
	go e.monitorOrder(ctx, closeOrder)
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
			if order.IsClose {
				// Market torn down while close order was pending.
				// Polymarket settles on-chain; close the ledger so it doesn't linger.
				e.ledger.ClosePosition(order.StrategyID, order.MarketID)
				e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
					StrategyID:  order.StrategyID,
					MarketID:    order.MarketID,
					Status:      strategy.StatusClosed,
					OpenOrderID: order.ID,
				})
			}
			return
		case <-deadline.C:
			// Timeout — cancel the order.
			e.log.Warn().Str("order_id", order.ID).Msg("order timed out; cancelling")
			if err := e.cancelOrder(ctx, order.ID); err != nil {
				e.log.Error().Err(err).Str("order_id", order.ID).Msg("failed to cancel order")
			}
			if order.IsClose {
				e.log.Warn().Str("order_id", order.ID).Str("market_id", order.MarketID).Msg("close order timed out; will retry on next crossover")
				// Close order timed out without filling — position is still open.
				e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
					StrategyID:  order.StrategyID,
					MarketID:    order.MarketID,
					Status:      strategy.StatusOpen,
					OpenOrderID: order.ID,
					OrderState:  strategy.OrderExpired,
				})
			} else {
				e.ledger.ClosePosition(order.StrategyID, order.MarketID)
				e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
					StrategyID:  order.StrategyID,
					MarketID:    order.MarketID,
					Status:      strategy.StatusClosed,
					Side:        order.Side,
					OpenOrderID: order.ID,
					OrderState:  strategy.OrderExpired,
				})
			}
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
				proceeds := order.Price.Mul(order.FilledSize)
				var pos *Position
				pnl := decimal.Zero
				if order.IsClose {
					pos = e.ledger.GetPosition(order.StrategyID, order.MarketID)
					if pos != nil {
						pnl = proceeds.Sub(pos.EntryPrice.Mul(pos.Size))
					}
					e.log.Info().
						Str("order_id", order.ID).
						Str("market_id", order.MarketID).
						Str("proceeds_usdc", proceeds.String()).
						Str("pnl_usdc", pnl.String()).
						Msg("close order filled")
				} else {
					e.log.Info().Str("order_id", order.ID).Msg("order filled")
				}
				e.mu.Lock()
				if order.IsClose {
					e.balance = e.balance.Add(proceeds)
				} else {
					e.balance = e.balance.Sub(proceeds)
				}
				e.mu.Unlock()
				if order.IsClose {
					e.ledger.ClosePosition(order.StrategyID, order.MarketID)
					if pos != nil {
						e.risk.RecordClose(order.StrategyID, order.MarketID, pos.Size, pnl)
					}
					e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
						StrategyID:  order.StrategyID,
						MarketID:    order.MarketID,
						Status:      strategy.StatusClosed,
						Side:        order.Side,
						OpenOrderID: order.ID,
						OrderState:  strategy.OrderFilled,
					})
				} else {
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
				}
				return
			}
			if state == OrderCancelled {
				e.log.Info().Str("order_id", order.ID).Msg("order cancelled")
				if order.IsClose {
					// Close order cancelled — position is still live.
					e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
						StrategyID:  order.StrategyID,
						MarketID:    order.MarketID,
						Status:      strategy.StatusOpen,
						OpenOrderID: order.ID,
						OrderState:  strategy.OrderCancelled,
					})
				} else {
					e.ledger.ClosePosition(order.StrategyID, order.MarketID)
					e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
						StrategyID:  order.StrategyID,
						MarketID:    order.MarketID,
						Status:      strategy.StatusClosed,
						Side:        order.Side,
						OpenOrderID: order.ID,
						OrderState:  strategy.OrderCancelled,
					})
				}
				return
			}
			if state == OrderPartialFill && !order.IsClose {
				switch e.cfg.PartialFillAction {
				case "cancel":
					if err := e.cancelOrder(ctx, order.ID); err != nil {
						e.log.Error().Err(err).Str("order_id", order.ID).Msg("cancel on partial fill failed")
					}
					if filled.IsPositive() {
						e.ledger.UpdatePositionSize(order.StrategyID, order.MarketID, filled)
						e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
							StrategyID:  order.StrategyID,
							MarketID:    order.MarketID,
							Status:      strategy.StatusOpen,
							Side:        order.Side,
							EntryPrice:  order.Price,
							Size:        filled,
							OpenOrderID: order.ID,
							OrderState:  strategy.OrderPartialFill,
						})
					} else {
						e.ledger.ClosePosition(order.StrategyID, order.MarketID)
						e.sendFeedback(order.StrategyID, strategy.PositionUpdate{
							StrategyID:  order.StrategyID,
							MarketID:    order.MarketID,
							Status:      strategy.StatusClosed,
							Side:        order.Side,
							OpenOrderID: order.ID,
							OrderState:  strategy.OrderCancelled,
						})
					}
					return
				default: // "keep" — continue polling for full fill
					e.ledger.UpdatePositionSize(order.StrategyID, order.MarketID, filled)
				}
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

// CloseMarket closes all open positions for a specific market.
func (e *LiveExecutor) CloseMarket(ctx context.Context, marketID string) error {
	positions := e.ledger.OpenPositions()
	for _, pos := range positions {
		if pos.MarketID != marketID {
			continue
		}
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
				Msg("close position failed during market teardown")
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
	// Polymarket enforces a 0.01 minimum tick size on price.
	price = price.Round(2)
	if price.IsZero() {
		price = decimal.NewFromFloat(0.01)
	}

	// BuyYes  → BUY  YES token (spend USDC)
	// BuyNo   → BUY  NO  token (spend USDC)
	// SellYes → SELL YES token (receive USDC) — used to exit a BuyYes position
	// SellNo  → SELL NO  token (receive USDC) — used to exit a BuyNo  position
	isSell := dir == strategy.SellYes || dir == strategy.SellNo
	sideInt := 0
	sideStr := "BUY"
	if isSell {
		sideInt = 1
		sideStr = "SELL"
	}
	buyNo := dir == strategy.BuyNo || dir == strategy.SellNo

	// CLOB precision rules:
	//   token amounts must be multiples of 10000 (2 decimal places at 1e6 scale)
	//   USDC amounts must be multiples of 100   (4 decimal places at 1e6 scale)
	// Use q = round(size*100) 0.01-token units so both constraints are satisfied
	// and makerAmount/takerAmount == price exactly.
	hundred := decimal.NewFromInt(100)
	priceCents := price.Mul(hundred).Round(0) // k = price in whole cents
	if priceCents.IsZero() {
		priceCents = decimal.NewFromInt(1)
	}
	q := size.Mul(hundred).Round(0) // number of 0.01-token units
	if q.IsZero() {
		q = decimal.NewFromInt(1)
	}
	tokenAmt := q.Mul(decimal.NewFromInt(10_000))  // multiple of 10000
	usdcAmt := q.Mul(priceCents).Mul(hundred)      // multiple of 100
	var makerAmount, takerAmount *big.Int
	if sideInt == 0 { // BUY: spend USDC (makerAmount), receive tokens (takerAmount)
		makerAmount = usdcAmt.BigInt()
		takerAmount = tokenAmt.BigInt()
	} else { // SELL: spend tokens (makerAmount), receive USDC (takerAmount)
		makerAmount = tokenAmt.BigInt()
		takerAmount = usdcAmt.BigInt()
	}

	// Resolve the correct token ID: YES token for BuyYes, NO token for BuyNo.
	// BuyNo places a BUY order on the NO token (USDC → NO), not a SELL on YES.
	tokenID := new(big.Int)
	if e.getMarketState != nil {
		_, _, tokenIDYes, tokenIDNo, ok := e.getMarketState(marketID)
		if ok {
			pick := tokenIDYes
			if buyNo && tokenIDNo != "" {
				pick = tokenIDNo
			}
			if pick != "" {
				tokenID.SetString(pick, 10)
			}
		}
	}
	if tokenID.Sign() == 0 {
		// Fallback: try parsing marketID directly (hex or decimal).
		tidStr := strings.TrimPrefix(marketID, "0x")
		if len(tidStr) < len(marketID) {
			tokenID.SetString(tidStr, 16)
		} else {
			tokenID.SetString(marketID, 10)
		}
		e.log.Warn().Str("market", marketID).Msg("YES token ID not in market state; falling back to marketID as tokenId — order may be rejected")
	}

	// Salt must fit in a JSON float64 (< 2^53) so the CLOB's JSON parser
	// doesn't lose precision before verifying the EIP-712 signature.
	var saltInt64 int64
	if err := binary.Read(crand.Reader, binary.BigEndian, &saltInt64); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	if saltInt64 < 0 {
		saltInt64 = -saltInt64
	}
	salt := new(big.Int).SetInt64(saltInt64 % (1 << 53))

	negRisk := e.resolveNegRisk(ctx, tokenID.String())
	feeRateBPS := e.resolveFeeRate(ctx, tokenID.String())
	e.log.Debug().Str("token_id", tokenID.String()).Bool("neg_risk", negRisk).Int("fee_rate_bps", feeRateBPS).Msg("market params resolved")

	// EIP-712 sign the order.
	var sigHex string
	if e.privateKey != nil {
		var err error
		sigHex, err = e.signOrder(orderSignInput{
			Salt:        salt,
			TokenID:     tokenID,
			MakerAmount: makerAmount,
			TakerAmount: takerAmount,
			Side:        big.NewInt(int64(sideInt)),
			FeeRateBPS:  feeRateBPS,
			NegRisk:     negRisk,
		})
		if err != nil {
			e.log.Warn().Err(err).Msg("EIP-712 order signing failed; order will be rejected")
		}
	}

	payload := map[string]interface{}{
		"order": map[string]interface{}{
			"salt":          json.RawMessage(salt.String()),
			"maker":         e.funderAddress,
			"signer":        e.walletAddress,
			"taker":         "0x0000000000000000000000000000000000000000",
			"tokenId":       tokenID.String(),
			"makerAmount":   makerAmount.String(),
			"takerAmount":   takerAmount.String(),
			"expiration":    "0",
			"nonce":         "0",
			"feeRateBps":    strconv.Itoa(feeRateBPS),
			"side":          sideStr,
			"signatureType": 2, // GNOSIS_SAFE
			"signature":     sigHex,
		},
		"owner":     e.connCfg.APIKey,
		"orderType": "GTC",
	}

	body, _ := json.Marshal(payload)
	bodyStr := string(body)
	e.log.Debug().Str("payload", bodyStr).Msg("submitting order")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.connCfg.RESTBaseURL+"/order", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.buildL2Headers("POST", "/order", bodyStr) {
		req.Header.Set(k, v)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP post /order: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var clobErr struct {
			Error string `json:"error"`      // ⚠️ verify field names against CLOB docs
			Code  string `json:"error_code"`
		}
		_ = json.Unmarshal(respBody, &clobErr)
		isBalanceError := clobErr.Code == "insufficient_balance" ||
			strings.Contains(strings.ToLower(clobErr.Error), "not enough balance")
		if isBalanceError {
			if bal, ferr := e.fetchBalance(context.Background()); ferr == nil {
				e.mu.Lock()
				e.balance = bal
				e.mu.Unlock()
			}
		}
		return "", fmt.Errorf("CLOB %d [%s]: %s (raw: %s)",
			resp.StatusCode, clobErr.Code, clobErr.Error, string(respBody))
	}

	var orderResp clobOrderResponse
	if err := json.Unmarshal(respBody, &orderResp); err != nil {
		return "", fmt.Errorf("parse order response: %w", err)
	}
	return orderResp.OrderID, nil
}

func (e *LiveExecutor) cancelOrder(ctx context.Context, orderID string) error {
	path := "/order/" + orderID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, e.connCfg.RESTBaseURL+path, nil)
	if err != nil {
		return err
	}
	for k, v := range e.buildL2Headers("DELETE", path, "") {
		req.Header.Set(k, v)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // already expired or filled — treat as success
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel order %s: status %d", orderID, resp.StatusCode)
	}
	return nil
}

func (e *LiveExecutor) checkOrderStatus(ctx context.Context, orderID string) (decimal.Decimal, OrderState, error) {
	path := "/data/order/" + orderID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.connCfg.RESTBaseURL+path, nil)
	if err != nil {
		return decimal.Zero, OrderPending, err
	}
	for k, v := range e.buildL2Headers("GET", path, "") {
		req.Header.Set(k, v)
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

// orderSignInput holds the typed values needed to sign a CTF Exchange order.
type orderSignInput struct {
	Salt        *big.Int
	TokenID     *big.Int
	MakerAmount *big.Int
	TakerAmount *big.Int
	Side        *big.Int // 0 = BUY, 1 = SELL
	FeeRateBPS  int
	NegRisk     bool
}

// signOrder computes the EIP-712 signature for a Polymarket CTF Exchange order.
// Domain: "Polymarket CTF Exchange" v1, chainId 137, verifyingContract = CTF Exchange on Polygon.
// Order struct matches the on-chain CTF Exchange contract definition.
func (e *LiveExecutor) signOrder(o orderSignInput) (string, error) {
	if e.privateKey == nil {
		return "", fmt.Errorf("no private key configured")
	}

	zero := big.NewInt(0)
	typedData := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Order": []apitypes.Type{
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
		PrimaryType: "Order",
		Domain: apitypes.TypedDataDomain{
			Name:    "Polymarket CTF Exchange",
			Version: "1",
			ChainId: gmath.NewHexOrDecimal256(137),
			VerifyingContract: func() string {
				if o.NegRisk {
					return "0xC5d563A36AE78145C45a50134d48A1215220f80a" // neg_risk exchange
				}
				return "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E" // standard exchange
			}(),
		},
		Message: apitypes.TypedDataMessage{
			"salt":          o.Salt,
			"maker":         e.funderAddress,
			"signer":        e.walletAddress,
			"taker":         "0x0000000000000000000000000000000000000000",
			"tokenId":       o.TokenID,
			"makerAmount":   o.MakerAmount,
			"takerAmount":   o.TakerAmount,
			"expiration":    zero,
			"nonce":         zero,
			"feeRateBps":    big.NewInt(int64(o.FeeRateBPS)),
			"side":          o.Side,
			"signatureType": big.NewInt(2), // GNOSIS_SAFE
		},
	}

	hash, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return "", fmt.Errorf("EIP-712 hash: %w", err)
	}

	// Sign the EIP-712 hash directly — do NOT use accounts.TextHash, which would
	// double-prefix an already-prefixed hash.
	sig, err := crypto.Sign(hash, e.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	sig[64] += 27 // adjust v to 27/28 as expected by Solidity ecrecover

	return fmt.Sprintf("0x%x", sig), nil
}

// buildL2Headers computes the HMAC-SHA256 L2 authentication headers required by
// Polymarket's CLOB API for all authenticated endpoints.
// message = timestamp + METHOD + requestPath + body (single quotes → double quotes)
// key     = base64url-decoded APISecret
// sig     = HMAC-SHA256(key, message) → base64url-encoded
func (e *LiveExecutor) buildL2Headers(method, path, body string) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := ts + strings.ToUpper(method) + path
	if body != "" {
		msg += strings.ReplaceAll(body, "'", `"`)
	}

	// Decode the API secret (base64url, possibly without padding).
	key, err := base64.RawURLEncoding.DecodeString(e.connCfg.APISecret)
	if err != nil {
		// Fall back to standard URL encoding with padding.
		key, _ = base64.URLEncoding.DecodeString(e.connCfg.APISecret)
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	sig := base64.URLEncoding.EncodeToString(mac.Sum(nil))
	sig = strings.NewReplacer("+", "-", "/", "_").Replace(sig)

	addr := e.walletAddress
	if addr == "" {
		addr = e.connCfg.APIKey // backward compat if no private key configured
	}
	return map[string]string{
		"POLY_ADDRESS":    addr,
		"POLY_SIGNATURE":  sig,
		"POLY_TIMESTAMP":  ts,
		"POLY_API_KEY":    e.connCfg.APIKey,
		"POLY_PASSPHRASE": e.connCfg.Passphrase,
	}
}

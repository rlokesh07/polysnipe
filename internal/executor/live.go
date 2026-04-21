package executor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
	getMarketState func(marketID string) (bid, ask decimal.Decimal, tokenIDYes string, ok bool)
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
	getMarketState func(marketID string) (bid, ask decimal.Decimal, tokenIDYes string, ok bool),
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
// It checks the real-time spread and returns an error if it exceeds MaxOrderSpreadCents.
// Falls back to sig.Price, then 0.5 with a warning.
func (e *LiveExecutor) resolveOrderPrice(marketID string, sigPrice decimal.Decimal) (decimal.Decimal, error) {
	if e.getMarketState != nil {
		bid, ask, _, ok := e.getMarketState(marketID)
		if ok && bid.IsPositive() && ask.IsPositive() {
			if e.cfg.MaxOrderSpreadCents > 0 {
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
	midPrice, err := e.resolveOrderPrice(sig.MarketID, sig.Price)
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

	midPrice, err := e.resolveOrderPrice(sig.MarketID, sig.Price)
	if err != nil {
		return err
	}
	offsetBPS := decimal.NewFromFloat(float64(e.cfg.DefaultLimitOffsetBPS) / 10000.0)
	var closePrice decimal.Decimal
	if closeDir == strategy.BuyYes {
		closePrice = midPrice.Add(midPrice.Mul(offsetBPS))
	} else {
		closePrice = midPrice.Sub(midPrice.Mul(offsetBPS))
	}
	orderID, err := e.submitOrder(ctx, sig.MarketID, closeDir, closePrice, pos.Size)
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
				cost := order.Price.Mul(order.FilledSize)
				e.mu.Lock()
				e.balance = e.balance.Sub(cost)
				e.mu.Unlock()
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
			if state == OrderPartialFill {
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
	sideInt := 0 // BUY YES tokens
	sideStr := "BUY"
	if dir == strategy.BuyNo {
		sideInt = 1 // SELL YES tokens (= buy NO)
		sideStr = "SELL"
	}

	// Scale USDC amounts to 6-decimal integer representation (Polygon USDC).
	// tokenId must be the YES token's uint256 ID from the CTF contract.
	// ⚠️ sig.MarketID should be the YES token ID (decimal string), not the condition ID.
	scale := decimal.NewFromInt(1_000_000)
	var makerAmt, takerAmt decimal.Decimal
	if sideInt == 0 { // BUY: spend USDC (makerAmount), receive YES tokens (takerAmount)
		makerAmt = size.Mul(scale)
		takerAmt = size.Div(price).Mul(scale)
	} else { // SELL: spend YES tokens (makerAmount), receive USDC (takerAmount)
		makerAmt = size.Div(price).Mul(scale)
		takerAmt = size.Mul(scale)
	}
	makerAmount := makerAmt.BigInt()
	takerAmount := takerAmt.BigInt()

	// Resolve YES token ID from the market state store (set by discovery engine).
	// Both BUY and SELL orders use the YES token ID; direction is indicated by the side field.
	tokenID := new(big.Int)
	if e.getMarketState != nil {
		_, _, tokenIDYes, ok := e.getMarketState(marketID)
		if ok && tokenIDYes != "" {
			tokenID.SetString(tokenIDYes, 10)
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

	// Random salt makes each order unique (required by CTF Exchange).
	saltBytes := make([]byte, 32)
	if _, err := crand.Read(saltBytes); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	salt := new(big.Int).SetBytes(saltBytes)

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
		if clobErr.Code == "insufficient_balance" {
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cancel order %s: status %d", orderID, resp.StatusCode)
	}
	return nil
}

func (e *LiveExecutor) checkOrderStatus(ctx context.Context, orderID string) (decimal.Decimal, OrderState, error) {
	path := "/order/" + orderID
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

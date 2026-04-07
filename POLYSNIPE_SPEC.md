# PolySnipe — Technical Specification

## Overview

PolySnipe is a multi-market prediction market trading bot built in Go, targeting Polymarket's CLOB (Central Limit Order Book) API. The bot watches real-time market data via WebSocket feeds, runs configurable trading strategies, and executes limit orders automatically. It supports both backtesting (against historical data fetched on demand) and live trading through the same code paths.

The primary target is Polymarket's 5-minute BTC prediction market, but the architecture supports multiple markets from day one.

---

## Architecture

### Goroutine Topology (Per Market)

```
[WebSocket Feed] ──> [Market State Engine] ──> [Strategy 1 Goroutine]
                                           ──> [Strategy 2 Goroutine]  ──> [Shared Signal Channel] ──> [Executor Goroutine]
                                           ──> [Strategy N Goroutine]
                                                     ^
                                                     |
                                          [Per-Strategy Feedback Channel] <── [Executor]
```

Each market spins up its own feed goroutine and market state engine. Strategy goroutines can be attached to one or more markets via config. The executor is shared globally across all markets.

Total goroutine count: `(1 feed + 1 state engine) * M markets + N strategy instances + 1 executor + 1 dashboard server`

### Data Flow

1. **Feed Goroutine**: Maintains WebSocket connection to Polymarket. Normalizes raw events into internal `MarketEvent` structs. Pushes to market state engine.
2. **Market State Engine**: Maintains rolling market state (best bid/ask, recent trades, order book depth, spread, time remaining). On each update, creates a snapshot struct (value copy, not pointer) and fans out to all strategy input channels for that market.
3. **Fan-out mechanism**: Each strategy has a buffered input channel (capacity 1). Send uses select/default — if the channel is full, drain the old snapshot and replace with the new one. Strategies always see the latest state, never process stale data.
4. **Strategy Goroutine**: Runs a select loop reading from two channels — market snapshots and position feedback. On each snapshot, evaluates its logic and optionally pushes a `Signal` onto the shared executor channel.
5. **Executor Goroutine**: Reads signals from the shared channel. Validates strategy ownership (a strategy can only close positions it opened). Applies Kelly criterion sizing against current balance. Places limit orders via Polymarket CLOB API. Monitors order lifecycle (fill, partial fill, timeout/cancel). Sends position updates back to the originating strategy via its dedicated feedback channel.

### Channel Definitions

| Channel | Direction | Type | Buffer | Notes |
|---|---|---|---|---|
| `marketSnapshotCh` | State Engine → Strategy | `chan MarketSnapshot` | 1 | Per-strategy, per-market. Drop-oldest on full. |
| `positionFeedbackCh` | Executor → Strategy | `chan PositionUpdate` | 1 | Per-strategy. Confirms fills, cancels, current position state. |
| `signalCh` | Strategy → Executor | `chan Signal` | 16 | Shared globally. Tagged with strategy ID + market ID. |

---

## Module Breakdown

### 1. Feed (`internal/feed/`)

Responsibilities:
- Establish and maintain WebSocket connection to Polymarket CLOB API
- Handle reconnection with configurable exponential backoff
- Parse incoming messages into `MarketEvent` structs
- Support multiple concurrent connections (one per market)
- In backtest mode: replaced by a replay feed that reads historical data fetched from Polymarket's API

Key types:
```go
type MarketEvent struct {
    MarketID    string
    Timestamp   time.Time
    EventType   string        // "trade", "book_update", "tick"
    BestBid     decimal.Decimal
    BestAsk     decimal.Decimal
    LastPrice   decimal.Decimal
    Volume      decimal.Decimal
    OrderBook   OrderBookSnapshot
}
```

### 2. Market State Engine (`internal/state/`)

Responsibilities:
- Consume `MarketEvent` stream from feed
- Maintain derived market state: spread, mid price, volume bars, time remaining in contract window
- Produce immutable `MarketSnapshot` structs on each update
- Fan out snapshots to all subscribed strategy channels using drop-oldest pattern

Key types:
```go
type MarketSnapshot struct {
    MarketID      string
    Timestamp     time.Time
    BestBid       decimal.Decimal
    BestAsk       decimal.Decimal
    Spread        decimal.Decimal
    MidPrice      decimal.Decimal
    LastPrice     decimal.Decimal
    Volume24h     decimal.Decimal
    TimeRemaining time.Duration
    OrderBook     OrderBookSnapshot
}
```

### 3. Strategy Interface (`internal/strategy/`)

```go
type Strategy interface {
    // ID returns the unique identifier for this strategy
    ID() string

    // Name returns the human-readable name
    Name() string

    // Run starts the strategy goroutine. It reads from snapshotCh,
    // receives position updates from feedbackCh, and pushes signals to signalCh.
    // It must respect the ctx for shutdown.
    Run(ctx context.Context, snapshotCh <-chan MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal)

    // Configure applies strategy-specific params from the config file.
    Configure(params map[string]interface{}) error

    // Markets returns the list of market IDs this strategy operates on.
    Markets() []string
}
```

Signal type:
```go
type Signal struct {
    StrategyID string
    MarketID   string
    Direction  Direction // BuyYes, BuyNo, Close
    Timestamp  time.Time
}

type Direction int
const (
    BuyYes Direction = iota
    BuyNo
    Close
)
```

Position update type:
```go
type PositionUpdate struct {
    StrategyID  string
    MarketID    string
    Status      PositionStatus // Open, Closed, None
    Side        Direction
    EntryPrice  decimal.Decimal
    Size        decimal.Decimal
    OpenOrderID string        // empty if no pending order
    OrderState  OrderState    // Pending, PartialFill, Filled, Cancelled, Expired
}
```

Constraint: One position per strategy per market. If a strategy already holds a position and emits BuyYes or BuyNo, the executor rejects it. The strategy must emit Close first.

### 4. Executor (`internal/executor/`)

Responsibilities:
- Read signals from the shared signal channel
- Validate strategy ownership before executing closes
- Apply Kelly criterion against current balance to determine position size
- Place limit orders via Polymarket CLOB API (EIP-712 signed)
- Monitor open orders — if unfilled after configurable timeout, cancel
- Track position ledger keyed by (strategy ID, market ID)
- Send position updates back to strategies via feedback channels
- Enforce all risk limits before order placement

Order lifecycle:
```
Signal received
  → Risk check (per-strategy, per-market, global limits)
  → Kelly sizing
  → Place limit order on CLOB
  → Monitor: filled / partial fill / timeout
  → On fill: update position ledger, send PositionUpdate to strategy
  → On timeout: cancel order, send PositionUpdate with OrderState=Cancelled
```

### 5. Risk Manager (`internal/risk/`)

The risk manager is consulted by the executor before every order. It enforces limits at three levels:

**Per-strategy limits:**
- `max_position_size` — max dollar value of a single position
- `max_open_orders` — max pending orders (should be 1 given single-position constraint)

**Per-market limits:**
- `max_exposure` — total dollar exposure across all strategies on this market
- `max_strategies_active` — how many strategies can hold positions simultaneously on this market

**Global limits:**
- `max_total_exposure` — across all markets and strategies
- `session_loss_limit` — if total P&L drops below this, halt all trading
- `max_daily_trades` — total order count cap per day
- `max_drawdown` — if drawdown from peak exceeds this, halt all trading

All values come from config. If any check fails, the signal is rejected and logged.

### 6. Kelly Criterion Sizer (`internal/sizing/`)

Determines position size based on:
- Current available balance
- Strategy's historical win rate (from execution logs)
- Average win/loss ratio
- Configurable Kelly fraction (full Kelly is aggressive; half-Kelly or quarter-Kelly is typical)

```go
type Sizer interface {
    Size(balance decimal.Decimal, strategyID string) decimal.Decimal
}
```

Falls back to a configurable fixed fraction of balance if insufficient historical data exists for Kelly calculation.

### 7. Backtester (`internal/backtest/`)

Responsibilities:
- Fetch historical market data from Polymarket API on demand
- Feed it through the same market state engine and strategy goroutines
- Replace the real executor with a simulated executor that:
  - Assumes limit orders fill at the specified price (configurable fill model)
  - Deducts fees using Polymarket's actual fee structure (rate configurable in YAML)
  - Tracks virtual portfolio, positions, and P&L
- Output: execution log (JSON), summary statistics (win rate, profit factor, max drawdown, Sharpe ratio)

The key design principle: **strategy code is identical in backtest and live mode.** The only difference is which executor implementation is injected.

### 8. Dashboard (`internal/dashboard/`)

Web-based dashboard served over HTTP. Full monitoring and control.

**Monitoring:**
- Active strategies and which markets they're running on
- Current positions per strategy per market (side, entry price, size, unrealized P&L)
- Open/pending orders and their age
- Execution log (recent signals, fills, cancels)
- P&L: per-strategy, per-market, global, session totals
- Bot health: WebSocket connection status per market, goroutine count, uptime, last error

**Controls:**
- Pause / resume individual strategies
- Kill a position (force close via market logic)
- Pause all trading (global halt)
- View current config (read-only — restart required to change)

Tech: Go `net/http` server, HTML/JS frontend, WebSocket push for live updates to the browser.

### 9. Orchestrator (`cmd/polysnipe/`)

The main entrypoint. Responsibilities:
- Parse config file
- Initialize all modules
- Wire up channels between feed → state engine → strategies → executor
- Register signal handlers for SIGTERM/SIGINT
- Start all goroutines
- Block until shutdown signal

**Shutdown sequence:**
1. Receive SIGTERM/SIGINT
2. Cancel context — all goroutines begin winding down
3. Executor cancels all open/pending orders
4. Executor closes all open positions (places close orders and waits for fill, with timeout)
5. Flush all logs
6. Exit

---

## Config File (`config.yaml`)

```yaml
# ============================================================
# PolySnipe Configuration
# Restart required to apply changes.
# ============================================================

# --- Connection ---
connection:
  websocket_url: "wss://ws-subscriptions-clob.polymarket.com/ws/market"
  rest_base_url: "https://clob.polymarket.com"
  api_key: "${POLYMARKET_API_KEY}"         # env var substitution
  api_secret: "${POLYMARKET_API_SECRET}"
  passphrase: "${POLYMARKET_PASSPHRASE}"
  reconnect_max_retries: 10
  reconnect_backoff_base_ms: 500
  reconnect_backoff_max_ms: 30000
  request_timeout_ms: 5000

# --- Markets ---
markets:
  - id: "btc-5min-up"
    condition_id: "0x..."
    token_id_yes: "0x..."
    token_id_no: "0x..."
    label: "BTC 5-Min Up"
  - id: "btc-5min-down"
    condition_id: "0x..."
    token_id_yes: "0x..."
    token_id_no: "0x..."
    label: "BTC 5-Min Down"
  # Add more markets as needed

# --- Execution ---
execution:
  order_type: "limit"                      # only "limit" supported
  default_limit_offset_bps: 10             # basis points from mid for limit price
  order_timeout_seconds: 30                # cancel unfilled orders after this
  partial_fill_action: "keep"              # "keep" or "cancel" on partial fill
  retry_on_failure: true
  retry_max_attempts: 3
  retry_backoff_ms: 200
  cooldown_between_orders_ms: 100

# --- Sizing (Kelly Criterion) ---
sizing:
  method: "kelly"                          # "kelly" or "fixed_fraction"
  kelly_fraction: 0.25                     # quarter-Kelly
  kelly_min_sample_size: 20                # trades needed before Kelly kicks in
  fixed_fraction_fallback: 0.02            # 2% of balance when insufficient data
  min_order_size: 1.0                      # minimum dollar order
  max_order_size: 500.0                    # maximum dollar order regardless of Kelly

# --- Risk Management ---
risk:
  global:
    max_total_exposure: 5000.0
    session_loss_limit: -500.0
    max_daily_trades: 200
    max_drawdown_pct: 15.0                 # halt if drawdown exceeds 15% from peak
  per_market:
    default:
      max_exposure: 2000.0
      max_strategies_active: 4
    overrides:                             # per-market overrides
      btc-5min-up:
        max_exposure: 3000.0
  per_strategy:
    default:
      max_position_size: 500.0
      max_open_orders: 1
    overrides:                             # per-strategy overrides
      spread_threshold:
        max_position_size: 200.0

# --- Strategies ---
strategies:
  spread_threshold:
    enabled: true
    markets: ["btc-5min-up", "btc-5min-down"]
    params:
      spread_trigger_bps: 50
      spread_exit_bps: 20
      side: "cheap"
  momentum:
    enabled: true
    markets: ["btc-5min-up"]
    params:
      consecutive_ticks: 5
      min_move_bps: 10
      hold_duration_seconds: 60
  time_decay:
    enabled: true
    markets: ["btc-5min-up", "btc-5min-down"]
    params:
      trigger_remaining_seconds: 30
      skew_threshold: 0.70
  # --- Stub slots for user-implemented strategies ---
  mid_flip:
    enabled: false
    markets: []
    params: {}
  reversal_snipe:
    enabled: false
    markets: []
    params: {}
  last_second_collapse:
    enabled: false
    markets: []
    params: {}
  strategy_7:
    enabled: false
    markets: []
    params: {}
  strategy_8:
    enabled: false
    markets: []
    params: {}

# --- Backtesting ---
backtest:
  enabled: false                           # set true to run in backtest mode
  start_date: "2025-01-01"
  end_date: "2025-03-01"
  starting_balance: 10000.0
  fee_rate_bps: 20                         # Polymarket fee rate in basis points
  data_cache_dir: "./data/historical"
  output_dir: "./data/backtest_results"
  fill_model: "optimistic"                 # "optimistic" or "midpoint"

# --- Logging ---
logging:
  level: "info"                            # debug, info, warn, error
  log_signals: true
  log_executions: true
  log_ticks: false
  output_dir: "./logs"
  format: "json"
  max_file_size_mb: 50
  max_files: 10

# --- Dashboard ---
dashboard:
  enabled: true
  host: "0.0.0.0"
  port: 8080
  auth_token: "${DASHBOARD_AUTH_TOKEN}"
  websocket_push_interval_ms: 500
  cors_allowed_origins: ["*"]
```

---

## Project Structure

```
polysnipe/
├── cmd/
│   └── polysnipe/
│       └── main.go                    # Entrypoint, orchestrator
├── internal/
│   ├── feed/
│   │   ├── websocket.go               # Live WebSocket feed
│   │   ├── replay.go                  # Historical data replay for backtesting
│   │   └── types.go                   # MarketEvent, OrderBookSnapshot
│   ├── state/
│   │   ├── engine.go                  # Market state engine, snapshot fan-out
│   │   └── types.go                   # MarketSnapshot
│   ├── strategy/
│   │   ├── interface.go               # Strategy interface definition
│   │   ├── types.go                   # Signal, Direction, PositionUpdate
│   │   ├── spread_threshold.go        # Example: Spread Threshold strategy
│   │   ├── momentum.go               # Example: Momentum strategy
│   │   └── time_decay.go             # Example: Time Decay strategy
│   ├── executor/
│   │   ├── live.go                    # Live CLOB executor
│   │   ├── simulated.go              # Backtest simulated executor
│   │   ├── ledger.go                 # Position ledger keyed by (strategy_id, market_id)
│   │   └── types.go                  # Order, Position, OrderState
│   ├── risk/
│   │   └── manager.go                # Risk checks at all three levels
│   ├── sizing/
│   │   ├── kelly.go                  # Kelly criterion implementation
│   │   └── fixed.go                  # Fixed fraction fallback
│   ├── backtest/
│   │   ├── runner.go                 # Backtest orchestration
│   │   ├── fetcher.go                # Historical data fetcher from Polymarket API
│   │   └── report.go                 # Summary stats generation
│   ├── dashboard/
│   │   ├── server.go                 # HTTP + WebSocket server
│   │   ├── handlers.go               # API endpoints (status, positions, controls)
│   │   └── static/                   # HTML/JS/CSS frontend
│   └── config/
│       ├── config.go                 # Config struct + YAML parsing
│       └── validate.go               # Config validation
├── config.yaml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## Dependencies

| Package | Purpose |
|---|---|
| `gorilla/websocket` | WebSocket client for Polymarket feed |
| `shopspring/decimal` | Precise decimal arithmetic for prices and sizing |
| `gopkg.in/yaml.v3` | Config file parsing |
| `ethereum/go-ethereum` | EIP-712 signing for Polymarket CLOB authentication |
| `rs/zerolog` | Structured JSON logging |

---

## Example Strategies (Included in Skeleton)

### Spread Threshold
When the bid-ask spread on a market exceeds a configurable threshold (in basis points), buy the cheaper side. Simple mean-reversion logic. Exits when spread contracts below a separate exit threshold.

Exercises: snapshot reading, signal emission, basic position management.

### Momentum
Track consecutive price ticks in the same direction. After N consecutive ticks (configurable) each moving at least M basis points, emit a signal following the trend. Exit after a configurable hold duration or on reversal.

Exercises: internal state tracking across ticks within the goroutine, time-based exit logic.

### Time Decay
Within the final N seconds of a 5-minute contract window, if the price is heavily skewed above a threshold (e.g., YES trading at 0.70+), bet against the skew. The thesis: late-window prices tend to overreact, and there's value in fading extreme positions near expiry.

Exercises: time-remaining field usage, contrarian signal logic.

---

## Backtest Output

Each backtest run produces:
- `executions.json` — every simulated fill with timestamp, strategy, market, side, price, size, fees
- `signals.json` — every signal fired, including rejected ones with rejection reason
- `summary.json` — aggregate stats:
  - Total trades, win rate, profit factor
  - Gross P&L, net P&L (after fees)
  - Max drawdown, max drawdown duration
  - Sharpe ratio (annualized)
  - Per-strategy breakdown of all above

---

## Shutdown Sequence

1. SIGTERM or SIGINT received
2. Context cancelled — all goroutines enter shutdown path
3. Feed goroutines close WebSocket connections
4. Strategy goroutines stop emitting signals, drain channels
5. Executor cancels all pending/open orders on Polymarket
6. Executor places close orders for all open positions
7. Executor waits for close fills (with configurable timeout — if timeout exceeded, log warning and exit anyway)
8. Flush all log files
9. Dashboard server shuts down
10. Process exits

---

## Claude Code Build Instructions

Build this as a well-structured Go project skeleton with fully working plumbing. The following should be operational out of the box:

1. Config parsing and validation
2. WebSocket feed connection and reconnection logic
3. Market state engine with snapshot fan-out
4. Strategy interface with all three example strategies implemented and functional
5. Executor with both live (CLOB API) and simulated (backtest) implementations
6. Position ledger with strategy ownership enforcement
7. Risk manager with all three levels of checks
8. Kelly criterion sizer with fixed-fraction fallback
9. Backtest runner that fetches historical data and produces output files
10. Dashboard with monitoring views and control endpoints
11. Graceful shutdown with position closing
12. Full config.yaml with all parameters documented

Strategy stubs for the remaining 5 user-implemented strategies should exist as disabled entries in config and empty files that implement the interface with TODO comments.

The bot should compile, run in backtest mode against sample data, and produce a valid backtest report with the three example strategies enabled.

# PolySnipe Modification Spec — Market Discovery Engine

## Overview

This modification replaces the static `markets:` config section with a dynamic market discovery engine. Instead of manually defining market IDs, condition IDs, and token IDs, the bot now continuously discovers markets from Polymarket's Gamma API based on configurable watchlists. Markets are automatically subscribed to, traded, and torn down as they appear, resolve, or close.

This turns the bot from "trade these specific markets" into "trade everything matching these criteria."

---

## What Changes

### Removed
- The `markets:` section in `config.yaml` (hardcoded market definitions)
- The `markets:` field on each strategy in config (hardcoded market assignment)

### Added
- Discovery engine module (`internal/discovery/`)
- Watchlist definitions in config
- Tag-based strategy-to-market matching
- Dynamic market lifecycle management (spin up / tear down goroutine stacks)
- Gamma API client (`internal/gamma/`)

### Modified
- **Strategy interface**: `Markets() []string` replaced with `Tags() []string`
- **Strategy config**: `markets: [...]` replaced with `tags: [...]`
- **Orchestrator**: No longer wires up markets at boot. Instead starts the discovery engine, which dynamically creates and destroys market goroutine stacks.
- **Dashboard**: New section showing discovered markets, attached strategies, and lifecycle events.
- **Risk manager**: Must handle dynamic market count — global exposure limits now apply across a potentially large and changing set of markets.

---

## Architecture

### New Goroutine Topology

```
[Discovery Engine Goroutine]
    │
    │ polls Gamma API every N seconds
    │ compares active markets against watchlist criteria
    │
    ├── New market found matching watchlist
    │     └── Spin up: [Feed] → [State Engine] → [Strategy 1..N Goroutines]
    │         (strategies attached based on tag matching)
    │
    ├── Market resolved / closed
    │     └── Tear down: cancel context for that market's goroutine stack
    │         executor closes any open positions / cancels orders
    │         all goroutines for that market exit
    │
    └── No change
          └── Sleep until next poll
```

The discovery engine is a single goroutine that runs for the lifetime of the bot. It manages a registry of active market stacks keyed by market ID.

### Market Lifecycle

```
Discovered → Subscribed → Active (trading) → Resolved/Closed → Torn Down
```

1. **Discovered**: Gamma API returns a market matching a watchlist's criteria.
2. **Subscribed**: Bot spins up WebSocket feed, market state engine, and strategy goroutines for this market. Each strategy that declared a matching tag gets an instance attached.
3. **Active**: Normal trading loop — strategies receive snapshots, emit signals, executor places orders.
4. **Resolved/Closed**: Discovery engine detects the market is no longer active on next poll. Initiates teardown.
5. **Torn Down**: All goroutines for this market are cancelled. If a strategy holds a position, the executor attempts to close it before teardown (same as graceful shutdown logic, scoped to one market). If the market has already resolved, positions are logged as resolved at settlement price. All channels are drained and closed.

### Discovery Engine → Orchestrator Interaction

The discovery engine doesn't spin up goroutines directly. It communicates with the orchestrator via a command channel:

```go
type MarketCommand struct {
    Action    MarketAction // Subscribe, Unsubscribe
    Market    MarketInfo
    Strategies []string    // strategy IDs matched by tags
}

type MarketAction int
const (
    Subscribe MarketAction = iota
    Unsubscribe
)
```

The orchestrator reads from this channel and handles the actual goroutine lifecycle. This keeps the discovery engine decoupled from the concurrency plumbing.

---

## New Module: Discovery Engine (`internal/discovery/`)

### Responsibilities
- Poll the Gamma API on a configurable interval
- Evaluate each active market against all configured watchlists
- Track which markets are currently subscribed (in-memory registry)
- Emit Subscribe commands for new matching markets
- Emit Unsubscribe commands for markets that have resolved, closed, or no longer match criteria
- Log all lifecycle events (discovered, subscribed, unsubscribed, resolved)

### Key Types

```go
type DiscoveryEngine struct {
    gamma        *gamma.Client
    watchlists   []Watchlist
    strategies   []Strategy       // to read Tags() from each
    registry     map[string]bool  // market ID → currently subscribed
    commandCh    chan<- MarketCommand
    pollInterval time.Duration
}

type Watchlist struct {
    Name    string
    Tags    []string          // required tags (AND logic)
    Filters PropertyFilters
}

type PropertyFilters struct {
    MaxExpiryMinutes  *int       // only markets expiring within N minutes
    MinExpiryMinutes  *int       // only markets with at least N minutes remaining
    OutcomeType       *string    // "binary" or empty for any
    MinVolume24h      *float64   // minimum 24h volume in dollars
    MinLiquidity      *float64   // minimum liquidity
    Active            bool       // only active/open markets (default true)
}
```

### Matching Logic

On each poll cycle:

1. Fetch all active markets from Gamma API (paginated if necessary)
2. For each market, fetch its tags
3. For each watchlist, check if the market's tags contain all of the watchlist's required tags AND the market passes all property filters
4. Collect all strategies whose declared tags overlap with the market's tags
5. If the market is new (not in registry) and has at least one matching strategy → emit Subscribe
6. If a market in the registry is no longer in the active set from Gamma → emit Unsubscribe

### Strategy-to-Market Matching

A strategy declares tags like `tags: ["crypto"]`. A discovered market has tags like `["crypto", "btc", "5-minute"]`. If any of the strategy's declared tags appear in the market's tag set, that strategy gets attached to that market.

This means:
- A strategy with `tags: ["crypto"]` matches every crypto market
- A strategy with `tags: ["nba"]` matches every NBA market
- A strategy with `tags: ["crypto", "nba"]` matches any market that has either tag (OR logic on strategy side)

---

## New Module: Gamma API Client (`internal/gamma/`)

### Responsibilities
- Wrap Polymarket's Gamma API (`https://gamma-api.polymarket.com`)
- No authentication required (fully public API)
- Provide typed methods for market discovery

### Endpoints Used

```go
type GammaClient interface {
    // ListMarkets returns active markets with optional filters
    // GET /markets?closed=false&limit=100&offset=0
    ListMarkets(ctx context.Context, opts ListMarketsOpts) ([]GammaMarket, error)

    // GetMarket returns a single market by condition ID
    // GET /markets/{conditionID}
    GetMarket(ctx context.Context, conditionID string) (*GammaMarket, error)

    // ListEvents returns events with optional filters
    // GET /events
    ListEvents(ctx context.Context, opts ListEventsOpts) ([]GammaEvent, error)

    // GetEventBySlug returns an event by its slug
    // GET /events/slug/{slug}
    GetEventBySlug(ctx context.Context, slug string) (*GammaEvent, error)

    // ListTags returns all available tags
    // GET /tags
    ListTags(ctx context.Context) ([]GammaTag, error)

    // SearchMarkets searches markets by keyword
    // GET /search?query={query}&type=markets
    SearchMarkets(ctx context.Context, query string) ([]GammaMarket, error)
}

type GammaMarket struct {
    ConditionID    string
    QuestionID     string
    Slug           string
    Question       string
    Description    string
    TokenIDs       []string       // [YES token ID, NO token ID]
    OutcomePrices  []string       // current prices
    Volume24h      float64
    Liquidity      float64
    Active         bool
    Closed         bool
    EndDate        time.Time
    Tags           []GammaTag
    EventID        string
}

type GammaTag struct {
    ID   string
    Slug string
    Name string
}

type ListMarketsOpts struct {
    Active    *bool
    Closed    *bool
    TagSlug   *string
    Limit     int
    Offset    int
    Order     string    // "volume24hr", "liquidity", "end_date", etc.
}
```

### Pagination

The Gamma API returns paginated results. The client must handle pagination transparently — the discovery engine calls `ListMarkets` and gets all results, not just the first page. Use limit/offset internally, default page size of 100.

### Rate Limiting

Respect Polymarket's API rate limits. The Gamma client should implement a simple rate limiter (configurable in YAML). Default to a conservative 10 requests/second since discovery is not latency-sensitive.

---

## Modified: Strategy Interface

### Before
```go
type Strategy interface {
    ID() string
    Name() string
    Run(ctx context.Context, snapshotCh <-chan MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal)
    Configure(params map[string]interface{}) error
    Markets() []string  // REMOVED
}
```

### After
```go
type Strategy interface {
    ID() string
    Name() string
    Run(ctx context.Context, snapshotCh <-chan MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal)
    Configure(params map[string]interface{}) error
    Tags() []string  // NEW — returns tags this strategy wants to trade
}
```

The `Tags()` method returns the list of tags from config. The discovery engine reads this to determine which strategies to attach when a new market is found.

---

## Modified: Config File

### Removed Section
```yaml
# REMOVED — no longer manually defined
markets:
  - id: "btc-5min-up"
    condition_id: "0x..."
    ...
```

### New Section
```yaml
# --- Market Discovery ---
discovery:
  enabled: true
  poll_interval_seconds: 60
  gamma_api_url: "https://gamma-api.polymarket.com"
  rate_limit_per_second: 10

  watchlists:
    - name: "crypto-short-expiry"
      tags: ["crypto"]
      filters:
        max_expiry_minutes: 10
        active: true

    - name: "nba-all"
      tags: ["nba"]
      filters:
        active: true

    - name: "politics"
      tags: ["politics"]
      filters:
        active: true
        min_volume_24h: 1000.0

    # Add more watchlists as needed
```

### Modified Strategy Section
```yaml
strategies:
  spread_threshold:
    enabled: true
    tags: ["crypto", "nba"]        # CHANGED from markets: [...]
    params:
      spread_trigger_bps: 50
      spread_exit_bps: 20
      side: "cheap"
  momentum:
    enabled: true
    tags: ["crypto"]               # CHANGED
    params:
      consecutive_ticks: 5
      min_move_bps: 10
      hold_duration_seconds: 60
  time_decay:
    enabled: true
    tags: ["crypto"]               # CHANGED
    params:
      trigger_remaining_seconds: 30
      skew_threshold: 0.70
  mid_flip:
    enabled: false
    tags: []
    params: {}
  reversal_snipe:
    enabled: false
    tags: []
    params: {}
  last_second_collapse:
    enabled: false
    tags: []
    params: {}
  strategy_7:
    enabled: false
    tags: []
    params: {}
  strategy_8:
    enabled: false
    tags: []
    params: {}
```

### Modified Risk Section
```yaml
risk:
  global:
    max_total_exposure: 5000.0
    session_loss_limit: -500.0
    max_daily_trades: 200
    max_drawdown_pct: 15.0
  per_market:
    default:
      max_exposure: 2000.0
      max_strategies_active: 4
    # per-market overrides now use tag patterns since market IDs are dynamic
    tag_overrides:
      crypto:
        max_exposure: 3000.0
      nba:
        max_exposure: 1000.0
  per_strategy:
    default:
      max_position_size: 500.0
      max_open_orders: 1
    overrides:
      spread_threshold:
        max_position_size: 200.0
```

---

## Modified: Orchestrator

### Before
The orchestrator read the `markets:` config, created a fixed set of feed + state engine + strategy goroutines at boot, and ran until shutdown.

### After
The orchestrator:
1. Parses config (watchlists, strategies with tags)
2. Starts the discovery engine goroutine
3. Reads MarketCommand events from the command channel
4. On **Subscribe**: spins up a MarketStack (feed, state engine, matched strategy goroutines) with its own cancellable context. Stores it in a registry keyed by market ID.
5. On **Unsubscribe**: cancels that market's context, triggering teardown. Executor closes positions for that market (if any). Waits for goroutines to exit. Removes from registry.
6. On shutdown: cancels all market contexts, runs normal graceful shutdown across everything.

```go
type MarketStack struct {
    MarketID   string
    Cancel     context.CancelFunc
    Strategies []string             // strategy IDs running on this market
    Done       chan struct{}         // closed when all goroutines have exited
}

type Orchestrator struct {
    registry   map[string]*MarketStack
    commandCh  <-chan MarketCommand
    // ... existing fields
}
```

---

## Modified: Dashboard

### New Dashboard Section: Market Discovery

**Monitoring:**
- Total discovered markets (matching watchlists)
- Currently subscribed markets with:
  - Market question/title
  - Tags
  - Attached strategies
  - Time since subscription
  - Current positions (if any)
- Recently resolved/torn down markets with final P&L
- Discovery engine status: last poll time, next poll time, markets found on last poll

**Lifecycle Log:**
- Timestamped feed of: market discovered, subscribed, strategy attached, market resolved, teardown initiated, teardown complete
- Filterable by tag, market ID, strategy

---

## Modified: Logging

New log events (JSON format, written to existing log files):

```json
{"level":"info","event":"market_discovered","market_id":"0x...","question":"Will BTC go up?","tags":["crypto","btc"],"matched_strategies":["momentum","spread_threshold"],"timestamp":"..."}
{"level":"info","event":"market_subscribed","market_id":"0x...","strategies_attached":3,"timestamp":"..."}
{"level":"info","event":"market_resolved","market_id":"0x...","outcome":"YES","positions_held":1,"realized_pnl":12.50,"timestamp":"..."}
{"level":"info","event":"market_teardown","market_id":"0x...","reason":"resolved","goroutines_stopped":5,"timestamp":"..."}
```

---

## Project Structure Changes

```
polysnipe/
├── internal/
│   ├── discovery/
│   │   ├── engine.go              # Discovery engine goroutine, poll loop, matching logic
│   │   ├── watchlist.go           # Watchlist evaluation, property filter matching
│   │   └── types.go               # MarketCommand, Watchlist, PropertyFilters
│   ├── gamma/
│   │   ├── client.go              # Gamma API HTTP client
│   │   ├── types.go               # GammaMarket, GammaTag, GammaEvent
│   │   └── pagination.go          # Transparent pagination handling
│   ├── strategy/
│   │   ├── interface.go           # MODIFIED — Markets() replaced with Tags()
│   │   └── ...
│   ├── executor/
│   │   └── ...                    # MODIFIED — teardown logic for single market
│   ├── dashboard/
│   │   └── ...                    # MODIFIED — new discovery monitoring section
│   └── config/
│       └── ...                    # MODIFIED — new discovery + watchlist config structs
```

---

## Teardown Sequence (Per Market)

When the discovery engine detects a market has resolved or closed:

1. Discovery engine emits `Unsubscribe` command
2. Orchestrator cancels that market's context
3. Feed goroutine closes WebSocket connection for that market, exits
4. State engine goroutine drains remaining events, exits
5. Strategy goroutines receive context cancellation, stop emitting signals
6. Executor checks if any strategy holds a position on this market:
   - If market resolved: log position as settled at resolution price, record P&L
   - If market closed but not resolved: attempt to close position (place close order with timeout)
   - If no position: nothing to do
7. All channels for this market are drained and closed
8. MarketStack.Done is closed, orchestrator removes from registry
9. Lifecycle event logged

---

## Edge Cases

**Market appears, disappears, reappears between polls**: The registry tracks by market ID. If a market is torn down and then re-discovered on a subsequent poll, it gets a fresh goroutine stack. No state carries over.

**Strategy holds position when market resolves**: Position is logged at settlement price. P&L is calculated based on entry price vs resolution outcome (YES = 1.0, NO = 0.0). This is a resolved gain/loss, not an active close.

**Gamma API goes down**: Discovery engine logs the error and retries on the next poll interval. Existing subscribed markets continue trading normally — discovery failure doesn't affect active market stacks.

**Hundreds of markets match**: No hard cap. The bot scales to whatever matches. Risk management is the safety net — global exposure limits prevent over-allocation even if 500 markets are active. Monitor goroutine count on the dashboard.

**Poll discovers market that's about to expire in seconds**: The strategy will receive snapshots and decide whether to trade. If time_remaining is too short for the strategy's logic, the strategy simply won't emit a signal. No special handling needed at the discovery layer.

---

## Claude Code Build Instructions

This is a modification to the existing PolySnipe codebase. The following changes should be implemented:

1. New `internal/discovery/` module with polling engine, watchlist evaluation, and market command emission
2. New `internal/gamma/` module with full Gamma API client, pagination, and rate limiting
3. Modified strategy interface: `Markets() []string` → `Tags() []string`
4. Modified config parsing: remove `markets:` section, add `discovery:` section with watchlists, change strategy `markets:` to `tags:`
5. Modified orchestrator: dynamic market lifecycle management via command channel and MarketStack registry
6. Modified executor: per-market teardown logic, handle position settlement on market resolution
7. Modified dashboard: new discovery monitoring section with lifecycle log
8. Modified risk manager: tag-based per-market overrides instead of market-ID overrides
9. Updated all three example strategies to use `Tags()` instead of `Markets()`
10. Updated config.yaml with discovery section and example watchlists

The bot should boot, discover markets from the Gamma API matching the configured watchlists, spin up trading stacks for each, and tear them down when markets resolve. The dashboard should show the full lifecycle in real time.

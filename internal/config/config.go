package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Connection ConnectionConfig          `yaml:"connection"`
	Discovery  DiscoveryConfig           `yaml:"discovery"`
	DryRun        bool    `yaml:"dry_run"`
	DryRunBalance float64 `yaml:"dry_run_balance"`
	Execution  ExecutionConfig           `yaml:"execution"`
	Sizing     SizingConfig              `yaml:"sizing"`
	Risk       RiskConfig                `yaml:"risk"`
	Strategies map[string]StrategyConfig `yaml:"strategies"`
	Backtest   BacktestConfig            `yaml:"backtest"`
	Logging    LoggingConfig             `yaml:"logging"`
	Dashboard  DashboardConfig           `yaml:"dashboard"`
}

type ConnectionConfig struct {
	WebSocketURL           string `yaml:"websocket_url"`
	RESTBaseURL            string `yaml:"rest_base_url"`
	WalletPrivateKey       string `yaml:"wallet_private_key"` // hex private key — EIP-712 order signing, derives POLY_ADDRESS
	APIKey                 string `yaml:"api_key"`            // derived Polymarket API key — POLY_API_KEY L2 header
	APISecret              string `yaml:"api_secret"`         // derived API secret (base64url) — HMAC L2 signing key
	Passphrase             string `yaml:"passphrase"`         // derived passphrase — POLY_PASSPHRASE L2 header
	ReconnectMaxRetries    int    `yaml:"reconnect_max_retries"`
	ReconnectBackoffBaseMS int    `yaml:"reconnect_backoff_base_ms"`
	ReconnectBackoffMaxMS  int    `yaml:"reconnect_backoff_max_ms"`
	RequestTimeoutMS       int    `yaml:"request_timeout_ms"`
}

// DiscoveryConfig controls the market discovery engine.
type DiscoveryConfig struct {
	Enabled            bool              `yaml:"enabled"`
	PollIntervalSec    int               `yaml:"poll_interval_seconds"`
	GammaAPIURL        string            `yaml:"gamma_api_url"`
	RateLimitPerSecond int               `yaml:"rate_limit_per_second"`
	MaxMarkets            int                  `yaml:"max_markets"`               // 0 = unlimited
	MaxSpreadCents        float64              `yaml:"max_spread_cents"`          // 0 = disabled; drops market if spread > this on first tick
	UpDownMaxSpreadCents  float64              `yaml:"updown_max_spread_cents"`   // spread threshold override for up-or-down markets (0 = use max_spread_cents)
	StaleMarketTimeoutSec       int                  `yaml:"stale_market_timeout_sec"`        // drop market if no price tick within this window (0 = disabled)
	UpDownStaleTimeoutSec       int                  `yaml:"updown_stale_timeout_sec"`        // stale timeout override for up-or-down markets (0 = use stale_market_timeout_sec)
	UpDownMarkets         UpDownMarketsConfig  `yaml:"updown_markets"`
	Watchlists            []WatchlistConfig    `yaml:"watchlists"`
}

// UpDownMarketsConfig configures the slug-based Up/Down crypto market discovery.
type UpDownMarketsConfig struct {
	Enabled bool     `yaml:"enabled"`
	Assets  []string `yaml:"assets"` // e.g. ["btc", "eth", "sol"] — must match slug prefixes
}

// WatchlistConfig is a single watchlist definition from config.
type WatchlistConfig struct {
	Name    string           `yaml:"name"`
	Tags    []string         `yaml:"tags"`
	Filters WatchlistFilters `yaml:"filters"`
}

// WatchlistFilters mirrors PropertyFilters for YAML parsing.
type WatchlistFilters struct {
	MaxExpiryMinutes *int     `yaml:"max_expiry_minutes"`
	MinExpiryMinutes *int     `yaml:"min_expiry_minutes"`
	OutcomeType      *string  `yaml:"outcome_type"`
	MinVolume24h     *float64 `yaml:"min_volume_24h"`
	MinLiquidity     *float64 `yaml:"min_liquidity"`
	Active           bool     `yaml:"active"`
	TitleContains    string   `yaml:"title_contains"`
}

type ExecutionConfig struct {
	OrderType               string `yaml:"order_type"`
	DefaultLimitOffsetBPS   int    `yaml:"default_limit_offset_bps"`
	OrderTimeoutSeconds     int    `yaml:"order_timeout_seconds"`
	PartialFillAction       string `yaml:"partial_fill_action"`
	RetryOnFailure          bool   `yaml:"retry_on_failure"`
	RetryMaxAttempts        int    `yaml:"retry_max_attempts"`
	RetryBackoffMS          int    `yaml:"retry_backoff_ms"`
	CooldownBetweenOrdersMS int     `yaml:"cooldown_between_orders_ms"`
	MaxOrderSpreadCents     float64 `yaml:"max_order_spread_cents"`
	FeeRateBPS              int     `yaml:"fee_rate_bps"`
}

type SizingConfig struct {
	Method                string  `yaml:"method"`
	KellyFraction         float64 `yaml:"kelly_fraction"`
	KellyMinSampleSize    int     `yaml:"kelly_min_sample_size"`
	FixedFractionFallback float64 `yaml:"fixed_fraction_fallback"`
	MinOrderSize          float64 `yaml:"min_order_size"`
	MaxOrderSize          float64 `yaml:"max_order_size"`
}

type RiskConfig struct {
	Global      GlobalRiskConfig      `yaml:"global"`
	PerMarket   PerMarketRiskConfig   `yaml:"per_market"`
	PerStrategy PerStrategyRiskConfig `yaml:"per_strategy"`
}

type GlobalRiskConfig struct {
	MaxTotalExposure float64 `yaml:"max_total_exposure"`
	SessionLossLimit float64 `yaml:"session_loss_limit"`
	MaxDailyTrades   int     `yaml:"max_daily_trades"`
	MaxDrawdownPct   float64 `yaml:"max_drawdown_pct"`
}

type MarketRiskLimits struct {
	MaxExposure         float64 `yaml:"max_exposure"`
	MaxStrategiesActive int     `yaml:"max_strategies_active"`
}

type PerMarketRiskConfig struct {
	Default      MarketRiskLimits            `yaml:"default"`
	TagOverrides map[string]MarketRiskLimits `yaml:"tag_overrides"`
}

type StrategyRiskLimits struct {
	MaxPositionSize float64 `yaml:"max_position_size"`
	MaxOpenOrders   int     `yaml:"max_open_orders"`
}

type PerStrategyRiskConfig struct {
	Default   StrategyRiskLimits            `yaml:"default"`
	Overrides map[string]StrategyRiskLimits `yaml:"overrides"`
}

type StrategyConfig struct {
	Enabled bool                   `yaml:"enabled"`
	Tags    []string               `yaml:"tags"`
	Params  map[string]interface{} `yaml:"params"`
}

type BacktestConfig struct {
	Enabled         bool    `yaml:"enabled"`
	StartDate       string  `yaml:"start_date"`
	EndDate         string  `yaml:"end_date"`
	StartingBalance float64 `yaml:"starting_balance"`
	FeeRateBPS      int     `yaml:"fee_rate_bps"`
	DataCacheDir    string  `yaml:"data_cache_dir"`
	OutputDir       string  `yaml:"output_dir"`
	FillModel       string  `yaml:"fill_model"`
}

type LoggingConfig struct {
	Level         string `yaml:"level"`
	LogSignals    bool   `yaml:"log_signals"`
	LogExecutions bool   `yaml:"log_executions"`
	LogTicks      bool   `yaml:"log_ticks"`
	OutputDir     string `yaml:"output_dir"`
	Format        string `yaml:"format"`
	MaxFileSizeMB int    `yaml:"max_file_size_mb"`
	MaxFiles      int    `yaml:"max_files"`
}

type DashboardConfig struct {
	Enabled                 bool     `yaml:"enabled"`
	Host                    string   `yaml:"host"`
	Port                    int      `yaml:"port"`
	AuthToken               string   `yaml:"auth_token"`
	WebSocketPushIntervalMS int      `yaml:"websocket_push_interval_ms"`
	CORSAllowedOrigins      []string `yaml:"cors_allowed_origins"`
}

// Load reads a config file and expands environment variables.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

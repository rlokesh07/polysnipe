package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Connection ConnectionConfig           `yaml:"connection"`
	Markets    []MarketConfig             `yaml:"markets"`
	Execution  ExecutionConfig            `yaml:"execution"`
	Sizing     SizingConfig               `yaml:"sizing"`
	Risk       RiskConfig                 `yaml:"risk"`
	Strategies map[string]StrategyConfig  `yaml:"strategies"`
	Backtest   BacktestConfig             `yaml:"backtest"`
	Logging    LoggingConfig              `yaml:"logging"`
	Dashboard  DashboardConfig            `yaml:"dashboard"`
}

type ConnectionConfig struct {
	WebSocketURL          string `yaml:"websocket_url"`
	RESTBaseURL           string `yaml:"rest_base_url"`
	APIKey                string `yaml:"api_key"`
	APISecret             string `yaml:"api_secret"`
	Passphrase            string `yaml:"passphrase"`
	ReconnectMaxRetries   int    `yaml:"reconnect_max_retries"`
	ReconnectBackoffBaseMS int   `yaml:"reconnect_backoff_base_ms"`
	ReconnectBackoffMaxMS  int   `yaml:"reconnect_backoff_max_ms"`
	RequestTimeoutMS      int    `yaml:"request_timeout_ms"`
}

type MarketConfig struct {
	ID          string `yaml:"id"`
	ConditionID string `yaml:"condition_id"`
	TokenIDYes  string `yaml:"token_id_yes"`
	TokenIDNo   string `yaml:"token_id_no"`
	Label       string `yaml:"label"`
}

type ExecutionConfig struct {
	OrderType               string `yaml:"order_type"`
	DefaultLimitOffsetBPS   int    `yaml:"default_limit_offset_bps"`
	OrderTimeoutSeconds     int    `yaml:"order_timeout_seconds"`
	PartialFillAction       string `yaml:"partial_fill_action"`
	RetryOnFailure          bool   `yaml:"retry_on_failure"`
	RetryMaxAttempts        int    `yaml:"retry_max_attempts"`
	RetryBackoffMS          int    `yaml:"retry_backoff_ms"`
	CooldownBetweenOrdersMS int    `yaml:"cooldown_between_orders_ms"`
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
	Default   MarketRiskLimits            `yaml:"default"`
	Overrides map[string]MarketRiskLimits `yaml:"overrides"`
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
	Markets []string               `yaml:"markets"`
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
	Enabled                bool     `yaml:"enabled"`
	Host                   string   `yaml:"host"`
	Port                   int      `yaml:"port"`
	AuthToken              string   `yaml:"auth_token"`
	WebSocketPushIntervalMS int     `yaml:"websocket_push_interval_ms"`
	CORSAllowedOrigins     []string `yaml:"cors_allowed_origins"`
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

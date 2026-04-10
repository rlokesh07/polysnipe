package config

import "fmt"

// Validate checks that the config is self-consistent and contains required fields.
func (c *Config) Validate() error {
	if c.Connection.WebSocketURL == "" {
		return fmt.Errorf("connection.websocket_url is required")
	}
	if c.Connection.RESTBaseURL == "" {
		return fmt.Errorf("connection.rest_base_url is required")
	}
	if c.DryRun && c.DryRunBalance <= 0 {
		return fmt.Errorf("dry_run_balance must be positive when dry_run is enabled")
	}
	if c.Discovery.Enabled {
		if c.Discovery.GammaAPIURL == "" {
			return fmt.Errorf("discovery.gamma_api_url is required when discovery is enabled")
		}
		if len(c.Discovery.Watchlists) == 0 {
			return fmt.Errorf("at least one watchlist must be configured when discovery is enabled")
		}
	}
	if c.Execution.OrderType != "limit" {
		return fmt.Errorf("execution.order_type must be \"limit\"; got %q", c.Execution.OrderType)
	}
	if c.Sizing.KellyFraction <= 0 || c.Sizing.KellyFraction > 1 {
		return fmt.Errorf("sizing.kelly_fraction must be in (0, 1]")
	}
	if c.Sizing.MinOrderSize <= 0 {
		return fmt.Errorf("sizing.min_order_size must be positive")
	}
	if c.Sizing.MaxOrderSize < c.Sizing.MinOrderSize {
		return fmt.Errorf("sizing.max_order_size must be >= min_order_size")
	}
	if c.Risk.Global.MaxTotalExposure <= 0 {
		return fmt.Errorf("risk.global.max_total_exposure must be positive")
	}
	if c.Backtest.Enabled {
		if c.Backtest.StartDate == "" {
			return fmt.Errorf("backtest.start_date is required when backtest is enabled")
		}
		if c.Backtest.EndDate == "" {
			return fmt.Errorf("backtest.end_date is required when backtest is enabled")
		}
		if c.Backtest.StartingBalance <= 0 {
			return fmt.Errorf("backtest.starting_balance must be positive")
		}
		if c.Backtest.FillModel != "optimistic" && c.Backtest.FillModel != "midpoint" {
			return fmt.Errorf("backtest.fill_model must be \"optimistic\" or \"midpoint\"")
		}
	}
	if c.Dashboard.Enabled && c.Dashboard.Port == 0 {
		return fmt.Errorf("dashboard.port is required when dashboard is enabled")
	}
	return nil
}

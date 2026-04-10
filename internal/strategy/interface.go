package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// Strategy is the interface that all trading strategies must implement.
type Strategy interface {
	// ID returns the unique identifier for this strategy instance.
	ID() string

	// Name returns the human-readable strategy name.
	Name() string

	// Run starts the strategy goroutine. It reads from snapshotCh,
	// receives position updates from feedbackCh, and pushes signals to signalCh.
	// It must return when ctx is cancelled.
	Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal)

	// Configure applies strategy-specific parameters from the config file.
	Configure(params map[string]interface{}) error

	// Tags returns the list of market tags this strategy wants to trade.
	// The discovery engine attaches the strategy to any market whose tag set
	// overlaps with this list (OR logic).
	Tags() []string
}

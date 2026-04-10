package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// Strategy7 is a stub strategy. Implement your logic here.
type Strategy7 struct {
	id   string
	tags []string
}

func NewStrategy7(id string) *Strategy7 { return &Strategy7{id: id} }

func (s *Strategy7) ID() string      { return s.id }
func (s *Strategy7) Name() string    { return "Strategy7" }
func (s *Strategy7) Tags() []string  { return s.tags }
func (s *Strategy7) SetTags(t []string) { s.tags = t }

func (s *Strategy7) Configure(params map[string]interface{}) error {
	// TODO: parse strategy-specific params
	return nil
}

func (s *Strategy7) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
	// TODO: implement strategy logic
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-snapshotCh:
			if !ok {
				return
			}
		case _, ok := <-feedbackCh:
			if !ok {
				return
			}
		}
	}
}

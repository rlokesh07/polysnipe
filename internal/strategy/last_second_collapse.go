package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// LastSecondCollapse is a stub strategy. Implement your logic here.
type LastSecondCollapse struct {
	id   string
	tags []string
}

func NewLastSecondCollapse(id string) *LastSecondCollapse { return &LastSecondCollapse{id: id} }

func (s *LastSecondCollapse) ID() string      { return s.id }
func (s *LastSecondCollapse) Name() string    { return "LastSecondCollapse" }
func (s *LastSecondCollapse) Tags() []string  { return s.tags }
func (s *LastSecondCollapse) SetTags(t []string) { s.tags = t }

func (s *LastSecondCollapse) Configure(params map[string]interface{}) error {
	// TODO: parse strategy-specific params
	return nil
}

func (s *LastSecondCollapse) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
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

package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// LastSecondCollapse is a stub strategy. Implement your logic here.
type LastSecondCollapse struct {
	id      string
	markets []string
}

func NewLastSecondCollapse(id string) *LastSecondCollapse { return &LastSecondCollapse{id: id} }

func (s *LastSecondCollapse) ID() string        { return s.id }
func (s *LastSecondCollapse) Name() string      { return "LastSecondCollapse" }
func (s *LastSecondCollapse) Markets() []string { return s.markets }
func (s *LastSecondCollapse) SetMarkets(m []string) { s.markets = m }

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

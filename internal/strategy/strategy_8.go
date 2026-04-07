package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// Strategy8 is a stub strategy. Implement your logic here.
type Strategy8 struct {
	id      string
	markets []string
}

func NewStrategy8(id string) *Strategy8 { return &Strategy8{id: id} }

func (s *Strategy8) ID() string        { return s.id }
func (s *Strategy8) Name() string      { return "Strategy8" }
func (s *Strategy8) Markets() []string { return s.markets }
func (s *Strategy8) SetMarkets(m []string) { s.markets = m }

func (s *Strategy8) Configure(params map[string]interface{}) error {
	// TODO: parse strategy-specific params
	return nil
}

func (s *Strategy8) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
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

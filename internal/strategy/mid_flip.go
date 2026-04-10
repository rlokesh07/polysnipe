package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// MidFlip is a stub strategy. Implement your logic here.
type MidFlip struct {
	id   string
	tags []string
}

func NewMidFlip(id string) *MidFlip { return &MidFlip{id: id} }

func (s *MidFlip) ID() string      { return s.id }
func (s *MidFlip) Name() string    { return "MidFlip" }
func (s *MidFlip) Tags() []string  { return s.tags }
func (s *MidFlip) SetTags(t []string) { s.tags = t }

func (s *MidFlip) Configure(params map[string]interface{}) error {
	// TODO: parse strategy-specific params
	return nil
}

func (s *MidFlip) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
	// TODO: implement strategy logic
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-snapshotCh:
			if !ok {
				return
			}
			// TODO: evaluate snapshot and emit signals
		case _, ok := <-feedbackCh:
			if !ok {
				return
			}
			// TODO: handle position updates
		}
	}
}

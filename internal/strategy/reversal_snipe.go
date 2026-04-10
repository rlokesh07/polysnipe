package strategy

import (
	"context"

	"polysnipe/internal/state"
)

// ReversalSnipe is a stub strategy. Implement your logic here.
type ReversalSnipe struct {
	id   string
	tags []string
}

func NewReversalSnipe(id string) *ReversalSnipe { return &ReversalSnipe{id: id} }

func (s *ReversalSnipe) ID() string      { return s.id }
func (s *ReversalSnipe) Name() string    { return "ReversalSnipe" }
func (s *ReversalSnipe) Tags() []string  { return s.tags }
func (s *ReversalSnipe) SetTags(t []string) { s.tags = t }

func (s *ReversalSnipe) Configure(params map[string]interface{}) error {
	// TODO: parse strategy-specific params
	return nil
}

func (s *ReversalSnipe) Run(ctx context.Context, snapshotCh <-chan state.MarketSnapshot, feedbackCh <-chan PositionUpdate, signalCh chan<- Signal) {
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

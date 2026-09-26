//go:build agent

package app

import (
	"context"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type workTurnDeltas struct {
	mu       sync.Mutex
	ctx      context.Context
	engine   *memql.MemQLEngine
	id, text string
	last     time.Time
	cancel   context.CancelCauseFunc
}

func (s *workTurnDeltas) TextDelta(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text += text
	if time.Since(s.last) < 200*time.Millisecond {
		return
	}
	s.last = time.Now()
	if err := s.engine.RecordWorkProgress(s.ctx, memql.WorkEvent{ID: s.id, Kind: "response", Phase: "streaming", Text: s.text}); err != nil {
		s.cancel(err)
	}
}
func (s *workTurnDeltas) ToolCall(string, string, string)   {}
func (s *workTurnDeltas) ToolResult(string, string, string) {}
func (s *workTurnDeltas) finish(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine.RecordWorkProgress(s.ctx, memql.WorkEvent{ID: s.id, Kind: "response", Phase: "completed", Text: text})
}

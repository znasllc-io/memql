//go:build agent

package app

import (
	"context"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/agent"
)

type workTurnDeltas struct {
	mu       sync.Mutex
	ctx      context.Context
	engine   *memql.MemQLEngine
	id, text string
	last     time.Time
	tools    map[string]memql.WorkEvent
	started  map[string]time.Time
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
func (s *workTurnDeltas) ToolCall(id, name, _ string) {
	// The response envelope is delivery, not an executed capability.
	if name == agent.RespondToUserToolName {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tools == nil {
		s.tools = map[string]memql.WorkEvent{}
		s.started = map[string]time.Time{}
	}
	event := memql.WorkEvent{ID: "tool-" + id, Kind: "action", Phase: "running", Name: name}
	s.tools[id], s.started[id] = event, time.Now()
	if err := s.engine.RecordWorkProgress(s.ctx, event); err != nil {
		s.cancel(err)
	}
}
func (s *workTurnDeltas) ToolResult(id, _ string, failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.tools[id]
	if !ok {
		return
	}
	event.Phase, event.ElapsedMS = "completed", time.Since(s.started[id]).Milliseconds()
	if failure != "" {
		event.Phase, event.Error = "failed", failure
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	if err := s.engine.RecordWorkProgress(ctx, event); err != nil {
		s.cancel(err)
	}
}
func (s *workTurnDeltas) finish(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine.RecordWorkProgress(s.ctx, memql.WorkEvent{ID: s.id, Kind: "response", Phase: "completed", Text: text})
}

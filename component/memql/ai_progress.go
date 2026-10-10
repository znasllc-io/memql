package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type aiProgressKey struct{}
type aiProgressSink func(string) error

// InvokeAIProgress is the opt-in public-output form of the ordinary prompt
// call. Only output text is recorded, never provider reasoning or metadata.
// Snapshots use the work journal, so another replica can serve a reconnect.
func (e *MemQLEngine) InvokeAIProgress(ctx context.Context, templateID string, data map[string]any) (any, error) {
	return e.invokePromptProgress(ctx, templateID, func(callCtx context.Context) (any, error) {
		return e.InvokeAI(callCtx, templateID, data)
	})
}

// Structured providers publish lifecycle and the completed public result. They
// never fall back to unconstrained streaming merely to provide partial output.
func (e *MemQLEngine) InvokeAIStructuredProgress(ctx context.Context, templateID string, data map[string]any, schemaName string, schema json.RawMessage, strict bool) (any, error) {
	return e.invokePromptProgress(ctx, templateID, func(callCtx context.Context) (any, error) {
		return e.InvokeAIStructured(callCtx, templateID, data, schemaName, schema, strict)
	})
}

func (e *MemQLEngine) invokePromptProgress(ctx context.Context, templateID string, invoke func(context.Context) (any, error)) (any, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.Mode == common.RunModeReplay {
		return invoke(ctx)
	}
	var latest string
	var last time.Time
	var savedBytes int
	record := func(text, phase string) error {
		return e.RecordWorkProgress(ctx, WorkEvent{ID: "ai-draft", Kind: "draft", Name: templateID, Text: text, Phase: phase})
	}
	if err := record("", "running"); err != nil {
		return nil, err
	}
	sink := aiProgressSink(func(text string) error {
		latest = text
		if !last.IsZero() && (time.Since(last) < 2*time.Second || len(text)-savedBytes < 1024) {
			return nil
		}
		last = time.Now()
		savedBytes = len(text)
		return record(text, "running")
	})
	result, err := invoke(context.WithValue(ctx, aiProgressKey{}, sink))
	phase := "completed"
	if err != nil {
		phase = "paused"
	} else if text, ok := result.(string); ok {
		latest = text
	}
	// A canceled model call must not discard the last public draft. One bounded
	// final write, with no goroutine or background timer left behind.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saveErr := e.RecordWorkProgress(recordCtx, WorkEvent{ID: "ai-draft", Kind: "draft", Name: templateID, Text: latest, Phase: phase})
	if err != nil {
		return nil, err
	}
	if saveErr != nil {
		return nil, saveErr
	}
	return result, nil
}

type promptStreamCaller struct {
	provider common.ChatStreamProvider
	sink     aiProgressSink
}

func (p promptStreamCaller) Call(ctx context.Context, prompt string) (any, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	chunks, err := p.provider.CallChatStream(streamCtx, []common.ChatMessage{{Role: "user", Content: prompt}})
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, open := <-chunks:
			if !open {
				return nil, fmt.Errorf("model stream ended before completion")
			}
			if chunk.Error != nil {
				return nil, chunk.Error
			}
			if text.Len()+len(chunk.Content) > 2*1024*1024 {
				return nil, fmt.Errorf("model draft exceeds output limit")
			}
			if chunk.Content != "" {
				text.WriteString(chunk.Content)
				if err = p.sink(text.String()); err != nil {
					return nil, err
				}
			}
			if chunk.Done {
				return text.String(), nil
			}
		}
	}
}
func (p promptStreamCaller) LastServed() (airoute.Served, bool) { return servedBy(p.provider) }

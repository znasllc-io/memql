package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type streamingContextFleet struct{ contextFleet }

func (f *streamingContextFleet) Call(_ context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.requests = append(f.requests, req)
	for _, m := range f.models {
		if m.ModelId == req.ModelId && m.ContextWindow < req.ContextTokens {
			return memql.FleetCallResult{}, &memql.FleetUnavailable{ModelId: req.ModelId, Considered: map[string]string{"laptop": fmt.Sprintf("context window %d is below %d", m.ContextWindow, req.ContextTokens)}, Total: 1}
		}
	}
	req.OnDelta("wide answer")
	return memql.FleetCallResult{Content: "wide answer"}, nil
}
func TestGrowingContextRetriesBeforeFirstStreamOutput(t *testing.T) {
	f := &streamingContextFleet{contextFleet{models: []memql.FleetModel{sizedModel("small", 3000000000, 8192), sizedModel("large", 8000000000, 65536)}}}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(f)
	r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"p": {memql.FleetFastest}}), testRules(t, defaultRule("p")), nil, nil)
	p, _, err := r.ResolveStreamWithTools(ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.CallChatStreamWithTools(context.Background(), []common.ChatMessage{{Role: "user", Content: strings.Repeat("x", 40000)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output string
	done := 0
	for c := range ch {
		if c.Error != nil {
			t.Errorf("unexpected stream error: %v", c.Error)
		}
		output += c.Content
		if c.Done {
			done++
		}
	}
	if len(f.requests) != 2 || f.requests[1].ModelId != "large" || output != "wide answer" || done != 1 {
		t.Fatalf("request count=%d output=%q done=%d", len(f.requests), output, done)
	}
	// One refusal, one fallback_used record, and one successful call.
	if got := r.RecordsDropped(); got != 3 {
		t.Fatalf("attempt ledger writes=%d, want 3 without duplicate observation", got)
	}
}

type refusalStream struct {
	chunks []common.StreamToolChunk
	calls  int
	wait   bool
}

func (p *refusalStream) Call(context.Context, string) (any, error) { return nil, nil }
func (p *refusalStream) CallChatStreamWithTools(ctx context.Context, _ []common.ChatMessage, _ []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	p.calls++
	ch := make(chan common.StreamToolChunk, len(p.chunks))
	if p.wait {
		go func() { <-ctx.Done(); close(ch) }()
		return ch, nil
	}
	for _, c := range p.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}
func refusalFallback(first, second *refusalStream) *fallbackStreamWithTools {
	providers := memql.NewProviderRegistryForTest()
	providers.RegisterForTest("first", "OpenAI", "first", first)
	providers.RegisterForTest("second", "OpenAI", "second", second)
	return &fallbackStreamWithTools{router: New(providers, nil, nil, nil, nil), chain: []string{"first", "second"}}
}
func TestStreamFallbackDoesNotReplayStartedCalls(t *testing.T) {
	refusal := &memql.FleetUnavailable{ModelId: "first"}
	raw := errors.New("runtime failed after accepting call")
	for _, tc := range []struct {
		name   string
		chunks []common.StreamToolChunk
		want   error
	}{
		{"raw before output", []common.StreamToolChunk{{Error: raw, Done: true}}, raw},
		{"typed after content", []common.StreamToolChunk{{Content: "partial"}, {Error: refusal, Done: true}}, refusal},
		{"typed after tools", []common.StreamToolChunk{{ToolCalls: []common.ToolCallDelta{{Name: "tool"}}}, {Error: refusal, Done: true}}, refusal},
		{"typed with content", []common.StreamToolChunk{{Content: "partial", Error: refusal, Done: true}}, refusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &refusalStream{chunks: tc.chunks}
			second := &refusalStream{chunks: []common.StreamToolChunk{{Content: "replayed", Done: true}}}
			ch, err := refusalFallback(first, second).CallChatStreamWithTools(context.Background(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got error
			count := 0
			for c := range ch {
				count++
				if c.Error != nil {
					got = c.Error
				}
			}
			if !errors.Is(got, tc.want) || second.calls != 0 || count != len(tc.chunks) {
				t.Fatalf("error=%v next calls=%d chunks=%d", got, second.calls, count)
			}
		})
	}
}
func TestStreamFallbackPreservesLastRefusalAndCancellation(t *testing.T) {
	t.Run("last refusal", func(t *testing.T) {
		refusal := &memql.FleetUnavailable{ModelId: "first"}
		first := &refusalStream{chunks: []common.StreamToolChunk{{Error: refusal, Done: true}}}
		f := refusalFallback(first, &refusalStream{})
		f.chain = f.chain[:1]
		ch, err := f.CallChatStreamWithTools(context.Background(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for c := range ch {
			count++
			if !errors.Is(c.Error, refusal) || !c.Done {
				t.Fatalf("chunk=%+v", c)
			}
		}
		if count != 1 {
			t.Fatalf("chunks=%d", count)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		first := &refusalStream{wait: true}
		second := &refusalStream{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := refusalFallback(first, second).CallChatStreamWithTools(ctx, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		timer := time.After(time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					if second.calls != 0 {
						t.Fatal("retry after cancel")
					}
					return
				}
			case <-timer:
				t.Fatal("stream did not close after cancel")
			}
		}
	})
}

// A STREAM RETRIED AFTER AN UNSTARTED REFUSAL NAMES THE REFUSED SOURCE on the
// row of the source that then served it (failure_reason.go): the refusal
// arrives as the first chunk, after the wrapper returned, so the retry is a
// second walk -- and the walk so far has to travel with it.
func TestAStreamRetriedAfterARefusalNamesTheRefusedSourceOnTheServedRow(t *testing.T) {
	first := &refusalStream{chunks: []common.StreamToolChunk{{Error: &memql.FleetUnavailable{ModelId: "first"}, Done: true}}}
	second := &refusalStream{chunks: []common.StreamToolChunk{{Content: "served", Done: true}}}
	f := refusalFallback(first, second)
	ledger := &countingLedger{writes: make(chan string, 8)}
	f.router.engine = ledger

	ch, err := f.CallChatStreamWithTools(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	rows := ledgerRows(t, ledger, 3)
	served, ok := rows["ok"]
	if !ok {
		t.Fatalf("no ok row among %v", rows)
	}
	entry, _ := consideredArg(served, "first")
	if line, _ := entry["reason"].(string); !strings.HasPrefix(line, "failed when called: "+memql.RefusalCodeNoLocalModel) {
		t.Errorf("the served row's line for the refused source is %q, want its failure and code", line)
	}
}

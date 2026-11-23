package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestPlainChatGrowingContextRetriesBeforeFirstOutput(t *testing.T) {
	f := &streamingContextFleet{contextFleet{models: []memql.FleetModel{sizedModel("small", 3000000000, 8192), sizedModel("large", 8000000000, 65536)}}}
	providers := registryOpenToEverybody()
	providers.SetFleetInference(f)
	r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"p": {memql.FleetFastest}}), testRules(t, defaultRule("p")), nil, nil)
	p, _, err := r.resolveStreamChat(context.Background(), ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.CallChatStream(context.Background(), []common.ChatMessage{{Role: "user", Content: strings.Repeat("x", 40000)}})
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

type plainRefusalStream struct {
	chunks []common.StreamChunk
	calls  int
	wait   bool
}

func (p *plainRefusalStream) Call(context.Context, string) (any, error) { return nil, nil }
func (p *plainRefusalStream) CallChatStream(ctx context.Context, _ []common.ChatMessage) (<-chan common.StreamChunk, error) {
	p.calls++
	ch := make(chan common.StreamChunk, len(p.chunks))
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
func plainRefusalFallback(first, second *plainRefusalStream) *fallbackStreamChat {
	providers := registryOpenToEverybody()
	providers.RegisterForTest("first", "OpenAI", "first", first)
	providers.RegisterForTest("second", "OpenAI", "second", second)
	return &fallbackStreamChat{router: New(providers, nil, nil, nil, nil), chain: []string{"first", "second"}}
}
func TestPlainChatFallbackDoesNotReplayStartedCalls(t *testing.T) {
	refusal := &memql.FleetUnavailable{ModelId: "first"}
	raw := errors.New("runtime failed after accepting call")
	for _, tc := range []struct {
		name   string
		chunks []common.StreamChunk
		want   error
	}{
		{"raw before output", []common.StreamChunk{{Error: raw, Done: true}}, raw},
		{"typed after content", []common.StreamChunk{{Content: "partial"}, {Error: refusal, Done: true}}, refusal},
		{"typed with content", []common.StreamChunk{{Content: "partial", Error: refusal, Done: true}}, refusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &plainRefusalStream{chunks: tc.chunks}
			second := &plainRefusalStream{chunks: []common.StreamChunk{{Content: "replayed", Done: true}}}
			ch, err := plainRefusalFallback(first, second).CallChatStream(clusterJob(), nil)
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
func TestPlainChatFallbackPreservesLastRefusalAndCancellation(t *testing.T) {
	t.Run("last refusal", func(t *testing.T) {
		refusal := &memql.FleetUnavailable{ModelId: "first"}
		first := &plainRefusalStream{chunks: []common.StreamChunk{{Error: refusal, Done: true}}}
		f := plainRefusalFallback(first, &plainRefusalStream{})
		f.chain = f.chain[:1]
		ch, err := f.CallChatStream(clusterJob(), nil)
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
		first := &plainRefusalStream{wait: true}
		second := &plainRefusalStream{}
		ctx, cancel := context.WithCancel(clusterJob())
		defer cancel()
		ch, err := plainRefusalFallback(first, second).CallChatStream(ctx, nil)
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

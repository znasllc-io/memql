package memql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type draftStream struct {
	chunks   []common.StreamChunk
	canceled <-chan struct{}
}

func (s *draftStream) CallChatStream(ctx context.Context, _ []common.ChatMessage) (<-chan common.StreamChunk, error) {
	s.canceled = ctx.Done()
	ch := make(chan common.StreamChunk, len(s.chunks))
	for _, v := range s.chunks {
		ch <- v
	}
	close(ch)
	return ch, nil
}
func TestPromptProgressStreamPreservesOnlyOutputAndRequiresCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []common.StreamChunk
		fail   bool
	}{
		{"complete", []common.StreamChunk{{Content: "hello", Metadata: map[string]any{"reasoning": "private"}}, {Content: " world", Done: true}}, false},
		{"closed", []common.StreamChunk{{Content: "partial"}}, true},
		{"error", []common.StreamChunk{{Content: "partial"}, {Error: errors.New("offline")}}, true},
		{"bounded", []common.StreamChunk{{Content: strings.Repeat("x", 2*1024*1024+1)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &draftStream{chunks: tc.chunks}
			var snapshots []string
			reply, err := (promptStreamCaller{provider: provider, sink: func(text string) error { snapshots = append(snapshots, text); return nil }}).Call(context.Background(), "prompt")
			if (err != nil) != tc.fail {
				t.Fatalf("reply=%v err=%v", reply, err)
			}
			if !tc.fail && (reply != "hello world" || len(snapshots) != 2 || snapshots[0] != "hello") {
				t.Fatalf("snapshots=%v reply=%v", snapshots, reply)
			}
			for _, s := range snapshots {
				if strings.Contains(s, "private") {
					t.Fatal("reasoning leaked")
				}
			}
			select {
			case <-provider.canceled:
			default:
				t.Fatal("provider lifetime outlived prompt")
			}
		})
	}
}
func TestPromptProgressStopsOnCancellationAndSnapshotFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (promptStreamCaller{provider: &draftStream{}, sink: func(string) error { return nil }}).Call(ctx, "")
	if err == nil {
		t.Fatal("canceled stream succeeded")
	}
	provider := &draftStream{chunks: []common.StreamChunk{{Content: "partial"}, {Content: "more", Done: true}}}
	calls := 0
	_, err = (promptStreamCaller{provider: provider, sink: func(string) error { calls++; return errors.New("snapshot failed") }}).Call(context.Background(), "")
	if err == nil || calls != 1 {
		t.Fatalf("continued after snapshot failure: %v %d", err, calls)
	}
}

func TestPromptProgressUsesRoutedStreamingAndTheExistingInvocation(t *testing.T) {
	e, _ := engineForAiBuiltin(t)
	stream := &draftStream{chunks: []common.StreamChunk{{Content: "draft"}, {Content: " complete", Done: true}}}
	e.aiRuntime.resolve = func(_ context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		if req.Modality != airoute.ModalityStreamingChat {
			t.Fatalf("wrong modality: %s", req.Modality)
		}
		return ResolvedProvider{Client: stream, Entry: &ProviderConfigEntry{Config: ProviderConfig{Name: "stub", Type: "test"}}, Resolution: airoute.Resolution{ProviderName: "stub"}}, nil
	}
	var latest string
	ctx := context.WithValue(context.Background(), aiProgressKey{}, aiProgressSink(func(s string) error { latest = s; return nil }))
	result, err := e.InvokeAI(ctx, "docSummaryProbe", map[string]any{"content": "evidence"})
	if err != nil || result != "draft complete" || latest != "draft complete" {
		t.Fatalf("result=%v err=%v latest=%s", result, err, latest)
	}
}

func TestStructuredPromptProgressPreservesSchemaRouting(t *testing.T) {
	e, model, seen := overrideSeamEngine(t, aiCacheConfig{})
	got, err := e.InvokeAIStructuredProgress(context.Background(), "draftReport", map[string]any{"week": "38"}, "draft", draftSchema, true)
	if err != nil || got != `{"draft":"the draft"}` || model.calls != 1 || len(*seen) != 1 || (*seen)[0].Modality != airoute.ModalityStructured {
		t.Fatalf("result=%v calls=%d resolutions=%v error=%v", got, model.calls, *seen, err)
	}
}

package memql

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// fakeParticipantResolver is a canned voiceParticipantResolver: it records
// the query it received and returns a GraphBundle-backed ExecuteResult (the
// only result form constructible from outside component/memql, and one of
// the shapes singleActiveHumanParticipantId handles).
type fakeParticipantResolver struct {
	mu       sync.Mutex
	gotQuery string
	bundle   *memqlv1.GraphBundle
	execErr  error
}

func (f *fakeParticipantResolver) CanonicalizeIdValue(_ context.Context, value, conceptType string) (string, error) {
	if strings.HasPrefix(value, "standard:") {
		return value, nil
	}
	return "standard:" + conceptType + ":" + value, nil
}

func (f *fakeParticipantResolver) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	f.gotQuery = query
	f.mu.Unlock()
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &memqlengine.ExecuteResult{Bundle: f.bundle}, nil
}

func (f *fakeParticipantResolver) ResolveSkills(_ context.Context, _ []string) (memqlengine.SkillBundle, error) {
	return memqlengine.SkillBundle{}, nil
}

func (f *fakeParticipantResolver) lastQuery() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotQuery
}

func participantBundleNode(id, participantType string) *memqlv1.MemoryNode {
	payload, _ := structpb.NewStruct(map[string]any{"participantType": participantType})
	return &memqlv1.MemoryNode{Id: id, Concept: "v1:cognition:participant", Payload: payload}
}

// TestResolveSingleActiveHumanParticipantId pins the #1403 server-side
// fallback rule end-to-end through the resolver seam: exactly one active
// human resolves to its participant id; zero or multiple (or a query
// failure) reject.
func TestResolveSingleActiveHumanParticipantId(t *testing.T) {
	ctx := context.Background()

	t.Run("single human resolves", func(t *testing.T) {
		fake := &fakeParticipantResolver{bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			participantBundleNode("standard:v1:cognition:participant:alice", "human"),
		}}}
		id, err := resolveSingleActiveHumanParticipantId(ctx, fake, "space-1")
		require.NoError(t, err)
		assert.Equal(t, "standard:v1:cognition:participant:alice", id)
		// The query must filter on the CANONICALIZED space id (participant
		// rows carry the canonical form; the voice-agent sends the bare slug).
		assert.Contains(t, fake.lastQuery(), `"standard:v1:cognition:space:space-1"`)
		assert.Contains(t, fake.lastQuery(), `participantType: "human"`)
		assert.Contains(t, fake.lastQuery(), `status: "active"`)
	})

	t.Run("no humans rejects", func(t *testing.T) {
		fake := &fakeParticipantResolver{bundle: &memqlv1.GraphBundle{}}
		_, err := resolveSingleActiveHumanParticipantId(ctx, fake, "space-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active human participant")
	})

	t.Run("multiple humans rejects as ambiguous", func(t *testing.T) {
		fake := &fakeParticipantResolver{bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			participantBundleNode("standard:v1:cognition:participant:alice", "human"),
			participantBundleNode("standard:v1:cognition:participant:bob", "human"),
		}}}
		_, err := resolveSingleActiveHumanParticipantId(ctx, fake, "space-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ambiguous")
	})

	t.Run("query failure rejects", func(t *testing.T) {
		fake := &fakeParticipantResolver{execErr: fmt.Errorf("db down")}
		_, err := resolveSingleActiveHumanParticipantId(ctx, fake, "space-1")
		require.Error(t, err)
	})

	t.Run("nil engine rejects", func(t *testing.T) {
		_, err := resolveSingleActiveHumanParticipantId(ctx, nil, "space-1")
		require.Error(t, err)
	})
}

// TestSingleActiveHumanParticipantId_RowMaps covers the shape-output row-map
// payload forms and the defensive filters (the shape runtime has been
// observed leaking non-matching nodes through a filtered query).
func TestSingleActiveHumanParticipantId_RowMaps(t *testing.T) {
	t.Run("flat shape row", func(t *testing.T) {
		id, err := singleActiveHumanParticipantId([]map[string]any{
			{"id": "standard:v1:cognition:participant:alice", "participantType": "human"},
		})
		require.NoError(t, err)
		assert.Equal(t, "standard:v1:cognition:participant:alice", id)
	})

	t.Run("nested payload row", func(t *testing.T) {
		id, err := singleActiveHumanParticipantId([]any{
			map[string]any{
				"id":      "standard:v1:cognition:participant:alice",
				"concept": "v1:cognition:participant",
				"payload": map[string]any{"participantType": "human"},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "standard:v1:cognition:participant:alice", id)
	})

	t.Run("leaked si and foreign-concept rows are filtered", func(t *testing.T) {
		id, err := singleActiveHumanParticipantId([]map[string]any{
			{"id": "standard:v1:cognition:participant:ga", "participantType": "si"},
			{"id": "standard:v1:agents:agent:x", "concept": "v1:agents:agent"},
			{"id": "standard:v1:cognition:participant:alice", "participantType": "human"},
		})
		require.NoError(t, err)
		assert.Equal(t, "standard:v1:cognition:participant:alice", id)
	})

	t.Run("duplicate rows collapse to one speaker", func(t *testing.T) {
		id, err := singleActiveHumanParticipantId([]map[string]any{
			{"id": "standard:v1:cognition:participant:alice", "participantType": "human"},
			{"id": "standard:v1:cognition:participant:alice", "participantType": "human"},
		})
		require.NoError(t, err)
		assert.Equal(t, "standard:v1:cognition:participant:alice", id)
	})

	t.Run("two distinct humans is ambiguous", func(t *testing.T) {
		_, err := singleActiveHumanParticipantId([]map[string]any{
			{"id": "standard:v1:cognition:participant:alice", "participantType": "human"},
			{"id": "standard:v1:cognition:participant:bob", "participantType": "human"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "2 active human participants")
	})

	t.Run("nil payload rejects", func(t *testing.T) {
		_, err := singleActiveHumanParticipantId(nil)
		require.Error(t, err)
	})
}

// capturingLogHandler collects rendered slog lines for assertion.
type capturingLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *capturingLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *capturingLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestHandleVoiceAgentFinalTranscript_MissingFieldsWarns pins the #1403
// no-more-silent-failures fix on the sync validation path: a final
// transcript missing required fields is rejected with a QueryError AND a
// WARN log naming the request + space + what was missing (previously the
// rejection logged nothing server-side).
func TestHandleVoiceAgentFinalTranscript_MissingFieldsWarns(t *testing.T) {
	buf := &capturingLogBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	svc := &service{logger: logger, engine: &memqlengine.MemQLEngine{}}
	stream := newRecordingClientStream(context.Background())
	sess := &streamSession{service: svc, stream: stream, logger: logger}

	err := sess.handleVoiceAgentFinalTranscript(
		&memqlv1.MemqlClientMessage{MessageId: "m1"},
		&memqlv1.VoiceAgentFinalTranscript{
			RequestId:     "r1",
			SpaceId:       "space-1",
			SpeakerUserId: "standard:v1:cognition:participant:alice",
			FinalText:     "   ", // missing
		})
	require.NoError(t, err)

	msgs := stream.snapshot()
	require.Len(t, msgs, 1)
	qe := msgs[0].GetQueryError()
	require.NotNil(t, qe, "missing finalText must reject with a QueryError")
	assert.Equal(t, "r1", qe.GetRequestId())

	out := buf.String()
	assert.Contains(t, out, "voice-agent final transcript rejected")
	assert.Contains(t, out, "space-1")
	assert.Contains(t, out, "missing_final_text=true")
}

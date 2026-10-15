package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func TestReadEvidenceObservationSurvivesResumeWithoutChangingRecovery(t *testing.T) {
	r, _, x, ctx, turn := recoveryHarness(t)
	messages := recoveryMessages(t, workRecoveryState{Version: 1})
	for n, args := range []string{`{"search":"missing regression"}`, `{"search":"regression missing"}`, `{"search":"test still absent"}`} {
		messages = append(messages, recoveryBatch(string(rune('a'+n)), "recallWorkHistory", args, `{"content":"same archived evidence"}`)...)
		var err error
		messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, nil)
		require.NoError(t, err)
	}
	state, marker, err := recoveryState(messages)
	require.NoError(t, err)
	require.Equal(t, 2, state.ReadEvidence.Repeated)
	require.False(t, state.ReviewActive)
	require.Zero(t, x.questions)
	// A fresh process gets only the saved checkpoint, no earlier transcript.
	r, _, x, ctx, turn = recoveryHarness(t)
	messages = []common.ChatMessage{messages[0], messages[marker]}
	messages = append(messages, recoveryBatch("new-page", "recallWorkHistory", `{"offset":100}`, `{"content":"new source evidence"}`)...)
	messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, nil)
	require.NoError(t, err)
	state, _, err = recoveryState(messages)
	require.NoError(t, err)
	require.Zero(t, state.ReadEvidence.Repeated)
	require.Equal(t, 2, state.ReadEvidence.Novel)
	require.Equal(t, "new-page", state.ReadEvidence.Recent[3].CallID)
	require.Zero(t, x.questions)
}

func TestReadEvidenceDoesNotTreatWritesOrUnknownToolsAsReads(t *testing.T) {
	var s workReadEvidence
	for _, action := range []string{"exec", "fs_write"} {
		s.observe("workerHost", "id", map[string]any{"action": action}, `{"ok":true}`)
	}
	s.observe("readAnything", "id", nil, "output")
	require.Empty(t, s.Recent)
	s.observe("workerHost", "id", map[string]any{"action": "fs_read"}, `{"content":"a","fileSize":1}`)
	s.observe("workerHost", "id2", map[string]any{"action": "fs_read"}, `{"fileSize":1,"content":"a"}`)
	require.Equal(t, 1, s.Repeated)
	s.observe("workerHost", "id3", map[string]any{"action": "fs_read"}, `{"content":"b","fileSize":1}`)
	require.Zero(t, s.Repeated)
}

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type replanSourceEngine struct {
	*workPromptEngine
	sources string
	reads   int
}

func (e *replanSourceEngine) ReadWorkReplanSources(context.Context) (string, error) {
	e.reads++
	return e.sources, nil
}
func TestOperationSourcesReachPreparedProviderRequestAfterNewBinding(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstreaming", true: "streaming"}[streaming], func(t *testing.T) {
			base, ctx, msg := trainingPromptFixture(t)
			e := &replanSourceEngine{workPromptEngine: base, sources: `{"runId":"run","recordedSources":[{"stepKey":"old-failed-step","value":{"fileId":"first","verified":true}}],"sourcesOmitted":true}`}
			r := newTestReplier(e)
			ctx, err := r.prepareReviewedWorkContext(ctx, msg)
			require.NoError(t, err)
			require.Equal(t, 1, e.reads)
			require.True(t, memql.HasWorkReplyTask(ctx))
			prepared, err := r.prepareTurn(ctx, msg, time.Now())
			require.NoError(t, err)
			provider := &outcomeProvider{steps: [][]common.ToolCall{nil}, texts: []string{"Retained first; reconcile omitted sources before the remainder."}}
			_, turn := progressRecoveryContext()
			if streaming {
				_, err = r.runStreamingToolLoop(ctx, provider, prepared.messages, nil, &captureSink{}, time.Now(), "replan-source", turn)
			} else {
				_, err = r.runNonStreamingToolLoop(ctx, provider, nil, prepared.messages, nil, &captureSink{}, time.Now(), "replan-source", turn)
			}
			require.NoError(t, err)
			require.Equal(t, 1, provider.calls)
			require.Contains(t, provider.messages[0].Content, `"fileId":"first"`)
			require.Contains(t, provider.messages[0].Content, `"sourcesOmitted":true`)
			require.Contains(t, provider.messages[0].Content, "Missing or omitted source content is unknown, never evidence that an effect did not happen")
		})
	}
}

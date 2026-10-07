package installation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// The real executor and forward authority handlers run on each side. Only
// the external Job/result store is shared by the two receiver instances.
type captureHopState struct {
	mu       sync.Mutex
	requests []pipelinesteps.StepRun
	result   []byte
	acked    int
	err      error
}
type captureHopRunner struct{ state *captureHopState }

func (r captureHopRunner) RunStep(_ context.Context, body []byte) []byte {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	var run pipelinesteps.StepRun
	if err := json.Unmarshal(body, &run); err != nil {
		r.state.err = err
		return nil
	}
	cfg := pipelinesteps.ConfigFromEnv(func(key string) string {
		if key == "MEMQL_PIPELINES_CLONE_IMAGE" {
			return "registry.example/clone@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}
		return ""
	})
	if _, err := pipelinesteps.BuildJob(cfg, run, pipelinesteps.JobName(run.RunID, run.StepKey, run.Attempt)); err != nil {
		r.state.err = err
		return nil
	}
	r.state.requests = append(r.state.requests, run)
	return r.state.result
}
func (r captureHopRunner) Status(context.Context, []byte) ([]byte, string) { return nil, "unused" }
func (r captureHopRunner) Ack(context.Context, []byte) string {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.acked++
	return ""
}
func (r captureHopRunner) CancelRun(context.Context, []byte) ([]byte, string) {
	return []byte(`{"jobsDeleted":0}`), ""
}
func (r captureHopRunner) Readiness(context.Context) ([]byte, string) { return nil, "unused" }

type captureHopForwarder struct {
	handler *workbench.ForwardHandler
	node    string
}

func (f captureHopForwarder) SelfNodeId() string   { return "capture-agent-" + f.node }
func (f captureHopForwarder) SelfNodeType() string { return "agent" }
func (f captureHopForwarder) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, _ string) (*nodev1.WorkbenchForwardResponse, string, error) {
	reply := make(chan *nodev1.WorkbenchForwardResponse, 1)
	// This receiver deliberately has none of the originating request context.
	f.handler.HandleForwardedRequest(context.Background(), req, func(msg *nodev1.NodeServerMessage) error { reply <- msg.GetWorkbenchForwardResponse(); return nil })
	select {
	case got := <-reply:
		return got, f.node, nil
	case <-ctx.Done():
		f.handler.CancelForwardedRequest(context.Background(), req.RequestId)
		return nil, f.node, ctx.Err()
	}
}
func (f captureHopForwarder) ForwardWatchedExcluding(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinned, excluded string, _ time.Duration, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error) {
	onSelected(f.node)
	return f.Forward(ctx, req, pinned)
}

func TestSourceCaptureUsesWorkbenchHopAndFreshReplicaRecovery(t *testing.T) {
	spec, body := captureFixture(t)
	first, err := newSourceCapture(spec)
	require.NoError(t, err)
	fixture, files := capturePorts(first, body)
	result, err := json.Marshal(fixture.result)
	require.NoError(t, err)
	state := &captureHopState{result: result}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	executor := func(node string) *pipelinesteps.Executor {
		handler := workbench.NewForwardHandler(nil, logger)
		handler.SetPipelineRunner(captureHopRunner{state: state})
		return pipelinesteps.NewExecutor(pipelinesteps.ConfigFromEnv(func(string) string { return "" }), captureHopForwarder{handler, node}, nil, logger)
	}
	ctx, cancel := context.WithTimeout(captureOperator(auth.RoleDeveloper, spec.OwnerUserID), 5*time.Second)
	defer cancel()
	receipt, err := first.run(ctx, executor("workbench-a"), false, "")
	require.NoError(t, err)
	peer, err := newSourceCapture(spec)
	require.NoError(t, err)
	second := executor("workbench-b")
	recovered, err := peer.run(ctx, second, true, "")
	require.NoError(t, err)
	require.Equal(t, receipt, recovered)
	verified, err := peer.verify(ctx, files, recovered)
	require.NoError(t, err)
	require.NotEmpty(t, verified.source.Digest())
	require.NoError(t, peer.acknowledge(ctx, second, recovered))
	state.mu.Lock()
	defer state.mu.Unlock()
	require.NoError(t, state.err)
	require.Len(t, state.requests, 2)
	require.Equal(t, 1, state.acked)
	a, b := state.requests[0], state.requests[1]
	require.False(t, a.RecoverOnly)
	require.True(t, b.RecoverOnly)
	require.Equal(t, a.Command, b.Command)
	require.Equal(t, a.SHA, b.SHA)
	require.Equal(t, a.Artifacts, b.Artifacts)
	require.Equal(t, a.RunDeadline, b.RunDeadline)
	require.Empty(t, b.Secrets)
	require.Empty(t, b.Caches)
	require.Empty(t, b.Services)
	require.Contains(t, b.Command, "/app/installation-source")
	require.NotContains(t, fmt.Sprint(verified), "private-source-contents")
}

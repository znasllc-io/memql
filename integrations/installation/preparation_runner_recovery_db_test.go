package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// The journal, Executor, ForwardHandler and Runner are real. Only the external
// Kubernetes API is a fixture. Recovery must read that API rather than infer an
// external dispatch from the journal's Started marker. TestMain and journalDB
// retain the normal mandatory-database checks.
type preparationRecoveryAPI struct {
	mu         sync.Mutex
	jobPath    string
	secretPath string
	jobBody    []byte
	requests   []string
}

func (a *preparationRecoveryAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	// A broken implementation cannot turn this bounded recovery fixture into a
	// polling loop or a creator. Unexpected calls are also asserted below.
	if len(a.requests) > 32 || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"kind":"Status","reason":"BadRequest","code":400}`)
		return
	}
	if r.URL.Path == a.jobPath && a.jobBody != nil {
		_, _ = w.Write(a.jobBody)
		return
	}
	if r.URL.Path == a.jobPath || r.URL.Path == a.secretPath {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, `{"kind":"Status","reason":"BadRequest","code":400}`)
}

func (a *preparationRecoveryAPI) reads(t *testing.T, jobs, secrets int) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	counts := make(map[string]int)
	for _, request := range a.requests {
		counts[request]++
	}
	want := map[string]int{http.MethodGet + " " + a.jobPath: jobs}
	if secrets != 0 {
		want[http.MethodGet+" "+a.secretPath] = secrets
	}
	require.Equal(t, want, counts, "recovery must make only the expected reads: no creates, patches, deletes or unrelated requests")
}

// The same JSON seam as the production workbench adapter, with observations
// of the real Runner's input/output. No outcome is synthesized here.
type preparationRecoveryReceiver struct {
	runner  *pipelinesteps.Runner
	mu      sync.Mutex
	runs    []pipelinesteps.StepRun
	results []pl.StepResult
}

func (a *preparationRecoveryReceiver) RunStep(ctx context.Context, body []byte) []byte {
	var run pipelinesteps.StepRun
	if json.Unmarshal(body, &run) != nil {
		return nil
	}
	result := a.runner.Run(ctx, run)
	a.mu.Lock()
	a.runs = append(a.runs, run)
	a.results = append(a.results, result)
	a.mu.Unlock()
	out, _ := json.Marshal(result)
	return out
}

func (a *preparationRecoveryReceiver) Status(ctx context.Context, body []byte) ([]byte, string) {
	var request pipelinesteps.StatusRequest
	if json.Unmarshal(body, &request) != nil {
		return nil, "decode_args"
	}
	out, _ := json.Marshal(a.runner.Status(ctx, request))
	return out, ""
}

func (a *preparationRecoveryReceiver) Ack(ctx context.Context, body []byte) string {
	var request pipelinesteps.AckRequest
	if json.Unmarshal(body, &request) != nil {
		return "decode_args"
	}
	if a.runner.Ack(ctx, request) != nil {
		return "ack_failed"
	}
	return ""
}

func (a *preparationRecoveryReceiver) CancelRun(ctx context.Context, body []byte) ([]byte, string) {
	var request pipelinesteps.CancelRequest
	if json.Unmarshal(body, &request) != nil {
		return nil, "decode_args"
	}
	n, err := a.runner.CancelRun(ctx, request)
	out, _ := json.Marshal(map[string]int{"jobsDeleted": n})
	if err != nil {
		return out, "cancel_failed"
	}
	return out, ""
}

func (a *preparationRecoveryReceiver) Readiness(context.Context) ([]byte, string) {
	out, _ := json.Marshal(a.runner.Readiness())
	return out, ""
}

func (a *preparationRecoveryReceiver) result(t *testing.T) pl.StepResult {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Len(t, a.runs, 1)
	require.Len(t, a.results, 1)
	require.True(t, a.runs[0].RecoverOnly)
	return a.results[0]
}

func preparationRealRecoveryExecutor(t *testing.T, server *httptest.Server, node string) (*pipelinesteps.Executor, *preparationRecoveryReceiver) {
	t.Helper()
	cfg := pipelinesteps.ConfigFromEnv(func(string) string { return "" })
	cfg.NodeID = node
	client := server.Client()
	client.Timeout = time.Second
	api := deploycontrol.NewClusterAPIWith(server.URL, "fixture-token", client)
	receiver := &preparationRecoveryReceiver{runner: pipelinesteps.NewRunner(cfg, pipelinesteps.NewKube(api, cfg.Namespace), nil, nil, nil)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := workbench.NewForwardHandler(nil, logger)
	handler.SetPipelineRunner(receiver)
	// captureHopForwarder drops the caller's in-process context at the hop.
	executor := pipelinesteps.NewExecutor(cfg, captureHopForwarder{handler: handler, node: node}, nil, logger)
	return executor, receiver
}

func requirePreparationHeadRetained(t *testing.T, ctx context.Context, db *sql.DB, journal *preparationJournal, record preparationRecord) {
	t.Helper()
	var preparation string
	var plan sql.NullString
	var epoch int64
	err := db.QueryRowContext(ctx, `SELECT active_preparation_id,active_plan_id,slot_epoch FROM installation_revision_heads WHERE installation_id=$1`, record.Scope.InstallationID).Scan(&preparation, &plan, &epoch)
	require.NoError(t, err)
	require.Equal(t, record.ID, preparation)
	require.False(t, plan.Valid)
	require.Equal(t, record.SlotEpoch, epoch)
	_, err = journal.cancelBeforeCapture(ctx, record.Scope.InstallationID, record.ID, record.Scope.WorkflowDigest)
	require.Error(t, err, "a committed dispatch intent cannot be released by pre-dispatch cancellation")
	next := testPlan()
	next.InstallationID, next.RequestedBy = record.Scope.InstallationID, record.Scope.RequestedBy
	_, err = (&revisionJournal{db: func() *sql.DB { return db }}).reserve(ctx, next)
	require.ErrorIs(t, err, errBusy, "another revision cannot take the installation while capture is unresolved")
}

func TestPreparationRecoveryUsesRealRunnerDurableEvidence(t *testing.T) {
	for _, savedOutcome := range []bool{true, false} {
		name := "no-job-or-queued-record"
		if savedOutcome {
			name = "persisted-job-outcome"
		}
		t.Run(name, func(t *testing.T) {
			db, peerDB := journalDB(t)
			first, peer := preparationConnection(db), preparationConnection(peerDB)
			scope, _ := preparationFixture(t)
			ctx, cancel := context.WithTimeout(captureOperator(auth.RoleOwner, scope.RequestedBy), 5*time.Second)
			defer cancel()
			record, err := first.reserve(ctx, scope)
			require.NoError(t, err)
			started, recoverOnly, err := first.beginCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate")
			require.NoError(t, err)
			require.False(t, recoverOnly)
			require.True(t, started.Captures["candidate"].Started)
			require.Nil(t, started.Captures["candidate"].Receipt)
			capture, err := newSourceCapture(scope.Captures["candidate"])
			require.NoError(t, err)
			jobName := pipelinesteps.JobName(capture.request.RunID, capture.request.StepKey, capture.request.Attempt)
			namespace := pipelinesteps.ConfigFromEnv(func(string) string { return "" }).Namespace
			api := &preparationRecoveryAPI{
				jobPath:    "/apis/batch/v1/namespaces/" + namespace + "/jobs/" + jobName,
				secretPath: "/api/v1/namespaces/" + namespace + "/secrets/" + pipelinesteps.SecretName(jobName),
			}
			intent := strings.Repeat("e", 64)
			if savedOutcome {
				// Model an external execution that completed before the original
				// driver died. Its durable outcome exists BEFORE either fresh runner.
				outcome, err := json.Marshal(pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: 0,
					Where: pl.Where{Surface: "cluster", JobName: jobName}, ArtifactIntentIDs: []string{intent}})
				require.NoError(t, err)
				api.jobBody, err = json.Marshal(pipelinesteps.Job{APIVersion: "batch/v1", Kind: "Job", Metadata: pipelinesteps.ObjectMeta{
					Name: jobName, Namespace: namespace, UID: "saved-job-uid", ResourceVersion: "7",
					Annotations: map[string]string{pipelinesteps.AnnotOutcome: string(outcome)},
				}})
				require.NoError(t, err)
			}
			server := httptest.NewServer(api)
			t.Cleanup(server.Close)
			firstExecutor, firstReceiver := preparationRealRecoveryExecutor(t, server, "preparation-workbench-a")
			// Lose this receiver's answer too: the journal still has only Started.
			firstReceipt, firstError := capture.run(ctx, firstExecutor, true, "")
			unchanged, err := peer.get(ctx, scope.InstallationID, record.ID)
			require.NoError(t, err)
			require.Equal(t, started, unchanged)
			secondExecutor, secondReceiver := preparationRealRecoveryExecutor(t, server, "preparation-workbench-b")
			recovered, recoveryError := peer.capture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", secondExecutor, "")
			for _, receiver := range []*preparationRecoveryReceiver{firstReceiver, secondReceiver} {
				result := receiver.result(t)
				if savedOutcome {
					require.Equal(t, pl.OutcomeSucceeded, result.Status)
					require.Nil(t, result.Failure)
					require.Equal(t, []string{intent}, result.ArtifactIntentIDs)
				} else {
					require.Equal(t, pl.OutcomeFailed, result.Status)
					require.NotNil(t, result.Failure)
					require.Equal(t, pl.CodeExecutionUncertain, result.Failure.Code)
				}
			}
			if savedOutcome {
				require.NoError(t, firstError)
				require.NoError(t, recoveryError)
				require.Equal(t, &firstReceipt, recovered.Captures["candidate"].Receipt)
				require.Equal(t, intent, firstReceipt.IntentID)
				require.Empty(t, recovered.Captures["candidate"].SourceDigest)
				require.False(t, recovered.Captures["candidate"].Acknowledged)
				stored, err := first.get(ctx, scope.InstallationID, record.ID)
				require.NoError(t, err)
				require.Equal(t, recovered, stored)
				api.reads(t, 2, 0)
			} else {
				require.Error(t, firstError)
				require.Error(t, recoveryError)
				stored, err := first.getByRequest(ctx, scope.InstallationID, scope.RequestID)
				require.NoError(t, err)
				require.Equal(t, started, stored, "uncertainty must not fabricate a receipt or clear Started")
				// Each fresh runner reads the Job, the queued Secret, then checks
				// the Job again before returning execution_uncertain.
				api.reads(t, 4, 2)
			}
			requirePreparationHeadRetained(t, ctx, peerDB, peer, started)
		})
	}
}

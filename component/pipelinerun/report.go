package pipelinerun

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/pipelines"
)

// The installed recipe chooses upload/observation order and polling cadence.
// These call-local operations are bound to one compiled report step. The native
// publication journal owns external-effect recovery across driver replicas.
func (dr *runDriver) runSARIFReport(ctx context.Context, t *stepTrack) {
	if !dr.stillHolds(ctx) {
		return
	}
	var err error
	t.handle, err = dr.work().Step(ctx, t.step.Key)
	if err != nil {
		dr.journalUnavailable(t.step.Key, err)
		return
	}
	s := t.snapshot()
	s.Status = StepRunning
	t.set(s)
	began := time.Now()
	finish := func(r receipt) { r.durationMs = max(time.Since(began).Milliseconds(), 1); dr.settle(ctx, t, r) }
	if t.step.SARIF == nil {
		finish(failReceipt(pipelines.CodeStepInvalid, "The report has no declared scanner artifact."))
		return
	}
	deadline := min(time.Duration(t.step.TimeoutSeconds)*time.Second, 10*time.Minute)
	if deadline <= 0 {
		deadline = 5 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(auth.ContextWithUserActor(ctx, dr.p.OwnerUserID), deadline)
	defer cancel()
	// Stop in-flight HTTP and storage operations as soon as the lease stops.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-dr.lease.stop:
			cancel()
		case <-done:
		}
	}()
	check := func() error {
		if callCtx.Err() != nil {
			return callCtx.Err()
		}
		if !dr.stillHolds(callCtx) || dr.lease.isCancelled() {
			return fmt.Errorf("report driver no longer holds its run")
		}
		return nil
	}
	var observed SARIFPublication
	result := func() map[string]any { return map[string]any{"state": observed.State, "publicationId": observed.ID} }
	_, err = workflowhost.Run(callCtx, "pipelineSARIFDelivery", nil, workflowhost.Options{
		Load: func(name string) (*automations.Automation, error) {
			if dr.workflow == nil || dr.workflow.definitions[name] == nil {
				return nil, fmt.Errorf("report recipe was not pinned")
			}
			return dr.workflow.definitions[name], nil
		},
		Operations: map[string]workflowhost.Operation{
			"pipelineReportPublish": func(ctx context.Context, args map[string]any) (any, error) {
				if len(args) != 0 {
					return nil, fmt.Errorf("report publication takes only its compiled scope")
				}
				if err := check(); err != nil {
					return nil, err
				}
				var err error
				observed, err = dr.i.PublishSARIF(ctx, dr.runID, t.step.SARIF.Step, t.step.SARIF.Path)
				return result(), err
			},
			"pipelineReportObserve": func(ctx context.Context, args map[string]any) (any, error) {
				seconds := rowInt(args, "waitSeconds")
				if seconds < 1 || seconds > 60 || len(args) != 1 || observed.ID == "" {
					return nil, fmt.Errorf("report observation requires a bound upload and a wait from 1 through 60 seconds")
				}
				if err := check(); err != nil {
					return nil, err
				}
				timer := time.NewTimer(time.Duration(seconds) * time.Second)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
				}
				if err := check(); err != nil {
					return nil, err
				}
				var err error
				observed, err = dr.i.ReadSARIF(ctx, observed.ID)
				return result(), err
			},
		},
	})
	if dr.lease.isLost() {
		return
	}
	if dr.lease.isCancelled() {
		finish(cancelReceipt())
		return
	}
	if err != nil {
		finish(failReceipt(pipelines.CodeSARIFDeliveryFailed, "Security report delivery could not be confirmed: "+dr.mask(err.Error())))
		return
	}
	if observed.State != "complete" || observed.ID == "" || observed.UploadID == "" || observed.SHA256 == "" {
		finish(failReceipt(pipelines.CodeSARIFUndelivered, "GitHub has not confirmed report ingestion ("+observed.State+"). Inspect the retained publication receipt before retrying."))
		return
	}
	finish(receipt{status: WorkStepDone, report: StepSucceeded, result: map[string]any{
		"status": string(pipelines.OutcomeSucceeded), "publicationId": observed.ID, "uploadId": observed.UploadID,
		"sha256": observed.SHA256, "ingestionStatus": observed.State,
	}})
}

//go:build agent

package app

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/core/id"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// The recovering agent has no originating executor or receipt in memory.
// Exercise its real dispatcher, including template resolution and the
// durable refusal, rather than only testing ResumeOptions in isolation.
func TestWorkDBAutomaticRecoveryDoesNotWaiveUncertainEffects(t *testing.T) {
	engine, db := workTemplateDBEngineAndDB(t)
	if err := engine.RegisterIntegration(workspine.New(engine, engine.Logger, func() *bun.DB { return db })); err != nil {
		t.Fatal(err)
	}
	key := "replay-effect-" + id.NewShortId()
	runID := "v1:work:run:" + key
	now := time.Now().UTC()
	loader := automations.NewLoader(automations.LoaderOptions{Functions: engine.Functions(), Logger: engine.Logger})
	template, err := loader.LoadByName("pollPipelines")
	if err != nil {
		t.Fatal(err)
	}
	insertWorkRow(t, db, runID, "v1:work:run", now.Add(-10*time.Minute), map[string]any{
		"automationName": "pollPipelines", "status": "running", "ownerUserId": "",
		"mode": "live", "triggeredBy": "schedule", "heartbeatAt": now.Add(-5 * time.Minute).Format(time.RFC3339Nano),
		"templateFingerprint": template.DefinitionFingerprint(id.New()), "startedAt": now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
	})
	insertWorkRow(t, db, "v1:work:step:"+key+"-polled", "v1:work:step", now.Add(-5*time.Minute), map[string]any{
		"runId": runID, "key": "polled", "seq": 0, "stepType": "function", "status": "running", "attempt": 1,
	})
	probe := &stepsRan{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: engine, StepRegistry: probe, Logger: engine.Logger})
	defer executor.Close()
	dispatcher := &workRunDispatcher{app: &App{engine: engine, Logger: logger,
		automationLoader: loader}, exec: executor}
	// An ownerless scheduler run is offered by the cluster-owner sweep.
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "recovery-test"))
	dispatcher.Dispatch(ctx, workspine.DispatchRequest{RunId: key, Status: "running", Recovery: true})
	if len(probe.ran) != 0 {
		t.Fatalf("automatic recovery repeated an uncertain call: %v", probe.ran)
	}
	var code string
	if err := db.NewRaw(`SELECT payload->>'errorCode' FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, runID).Scan(context.Background(), &code); err != nil {
		t.Fatal(err)
	}
	if code != workResumeEffectUncertain {
		t.Fatalf("stored failure=%q; expected explicit uncertainty, not silent abandonment: %s", code, logs.String())
	}
}

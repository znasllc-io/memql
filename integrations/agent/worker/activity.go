package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// The holding replica writes the same lifecycle for local and forwarded calls.
// Optional on small test stores; the engine store always implements it. These
// receipts contain no prompt, arguments, output or borrowed user's identity.
type activityStore interface {
	writeMachineActivity(context.Context, string, map[string]any) error
}
type machineActivity struct {
	store  activityStore
	ctx    context.Context
	logger *slog.Logger
	id     string
	done   bool
}

func beginMachineActivity(ctx context.Context, store any, logger *slog.Logger, owner, worker, kind, label, correlation string, timeout time.Duration) *machineActivity {
	a := &machineActivity{logger: logger}
	a.store, _ = store.(activityStore)
	if a.store == nil || owner == "" {
		return a
	}
	a.ctx = auth.ContextWithUserActor(context.WithoutCancel(ctx), owner)
	a.id = "v1:worker:activity:" + id.NewShortId()
	now := time.Now().UTC()
	a.write("createMachineActivity", map[string]any{"activityId": a.id, "workerId": worker, "kind": kind, "label": label, "correlationId": correlation, "startedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(timeout + time.Minute).Format(time.RFC3339Nano)})
	return a
}
func (a *machineActivity) write(name string, args map[string]any) {
	if a.store == nil || a.id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	if err := a.store.writeMachineActivity(ctx, name, args); err != nil && a.logger != nil {
		a.logger.Warn("machine activity persistence failed", "operation", name, "error", err)
	}
}
func (a *machineActivity) running() {
	a.write("updateMachineActivity", map[string]any{"activityId": a.id, "status": "running"})
}
func (a *machineActivity) end(status string) {
	if a.done {
		return
	}
	a.done = true
	a.write("updateMachineActivity", map[string]any{"activityId": a.id, "status": status, "endedAt": time.Now().UTC().Format(time.RFC3339Nano)})
}
func (a *machineActivity) interrupted(ctx context.Context) {
	status := "failed"
	if ctx.Err() != nil {
		status = "cancelled"
	}
	a.end(status)
}
func (s *EngineStore) writeMachineActivity(ctx context.Context, name string, args map[string]any) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return err
	}
	_, err = s.Engine.Execute(ctx, call)
	return err
}

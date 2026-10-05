package automations

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrJournalRequired means execution stopped at an unconfirmed journal
// boundary. The last effect may have happened; callers must reconcile it
// before replaying. It is not an ordinary step error that retry or an error
// handler can safely ignore.
var ErrJournalRequired = errors.New("required work journal unavailable")

type journalRequirement struct {
	mu     sync.Mutex
	err    error
	cancel context.CancelCauseFunc
}

type journalRequirementKey struct{}

func prepareJournalContract(a *Automation) error {
	if a.JournalRequired && (a.BeforeWrite != nil || journalSkipsAutomation(a)) {
		return fmt.Errorf("automation %s: @journalRequired cannot be used on a before-write hook or an automation triggered by work-journal rows", a.Name)
	}
	return nil
}

func requiredJournalError(ctx context.Context) error {
	r, _ := ctx.Value(journalRequirementKey{}).(*journalRequirement)
	return r.failure()
}

func (r *journalRequirement) failure() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *journalRequirement) fail(name string, err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = fmt.Errorf("%w: %s: %w", ErrJournalRequired, name, err)
		r.cancel(r.err)
	}
}

// Each execution gets its own requirement; the executor's shared journal
// remains unchanged. Descendant logic, branches and child automations inherit
// it so a failed nested receipt stops the entire owning execution. Sandboxed
// previews still write nothing and do not claim durable execution.
func (e *Executor) requireRunJournal(ctx context.Context, a *Automation, j *workJournal) (context.Context, *workJournal, func(), error) {
	stop := func() {}
	r, _ := ctx.Value(journalRequirementKey{}).(*journalRequirement)
	if e.sandboxRun || (!a.JournalRequired && r == nil) {
		return ctx, j, stop, nil
	}
	if j == nil {
		err := fmt.Errorf("%w: automation %s has no journal", ErrJournalRequired, a.Name)
		r.fail("openRun", err)
		return ctx, nil, stop, err
	}
	if r == nil {
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		r = &journalRequirement{cancel: cancel}
		ctx = context.WithValue(ctx, journalRequirementKey{}, r)
		stop = func() { cancel(nil) }
	}
	copy := *j
	copy.required = r
	return ctx, &copy, stop, r.failure()
}

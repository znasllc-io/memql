package pipelinesteps

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/core/component"
)

// Maintenance only reconciles resources already owned by the runner. It does
// not create Jobs or require a connected pipeline source to make progress.
type Maintenance struct {
	*component.Component
	runner                      *Runner
	interval, continuationDelay time.Duration
}

func NewMaintenance(runner *Runner) *Maintenance {
	comp, _ := component.New("pipelines.maintenance")
	m := &Maintenance{Component: comp, runner: runner, interval: reapInterval, continuationDelay: time.Second}
	_ = m.ConfigureLifecycle(component.WithRunHook(m.run))
	return m
}

func (m *Maintenance) run(ctx context.Context, markStarted func()) error {
	markStarted()
	backoff := m.continuationDelay
	lastError := ""
	for ctx.Err() == nil {
		_, more, err := m.runner.reap(ctx)
		delay := m.interval
		if more {
			delay = m.continuationDelay
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err.Error() != lastError {
				m.runner.log.Warn("pipelines: orphan cleanup pending; the next sweep will retry", "error", err)
				lastError = err.Error()
			}
			delay = backoff
			backoff = min(backoff*2, time.Minute)
		} else {
			backoff = m.continuationDelay
			lastError = ""
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

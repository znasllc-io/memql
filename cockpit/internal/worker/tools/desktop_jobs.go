package tools

import (
	"context"
	"fmt"
	"sync"
)

type desktopJob struct {
	ctx    context.Context
	run    func() error
	result chan error
}

func (j desktopJob) execute() {
	if err := j.ctx.Err(); err != nil {
		j.result <- err
		return
	}
	j.result <- j.run()
}

// Unbuffered handoff means cancellation before acceptance leaves no queued
// activation. Once accepted, the caller waits for completion, even if cancelled:
// no delayed job can activate an app after its tool call has returned.
type desktopJobs struct {
	mu      sync.Mutex
	queue   chan desktopJob
	stopped chan struct{}
}

func (d *desktopJobs) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queue = make(chan desktopJob)
	d.stopped = make(chan struct{})
}
func (d *desktopJobs) stop() { d.mu.Lock(); defer d.mu.Unlock(); close(d.stopped); d.queue = nil }
func (d *desktopJobs) call(ctx context.Context, run func() error) error {
	d.mu.Lock()
	queue, stopped := d.queue, d.stopped
	d.mu.Unlock()
	if queue == nil {
		return fmt.Errorf("desktop main loop is not running")
	}
	job := desktopJob{ctx: ctx, run: run, result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stopped:
		return fmt.Errorf("desktop main loop stopped")
	case queue <- job:
		return <-job.result
	}
}

var desktopMainJobs desktopJobs

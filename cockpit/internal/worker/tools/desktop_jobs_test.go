package tools

import (
	"context"
	"errors"
	"testing"
)

func TestDesktopJobCancellationCannotLeaveLateActivation(t *testing.T) {
	d := &desktopJobs{}
	d.start()
	defer d.stop()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	called := false
	go func() { result <- d.call(ctx, func() error { called = true; return nil }) }()
	job := <-d.queue // accepted, but main-thread execution has not begun
	cancel()
	job.execute()
	if err := <-result; !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled job executed: %v", err)
	}
}

func TestDesktopJobWaitsForAcceptedOperationAndStopsQueuedCalls(t *testing.T) {
	d := &desktopJobs{}
	d.start()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- d.call(ctx, func() error { return nil }) }()
	job := <-d.queue
	cancel()
	select {
	case <-result:
		t.Fatal("returned while accepted operation could still execute")
	default:
	}
	job.execute()
	<-result
	go func() {
		result <- d.call(context.Background(), func() error { t.Error("job executed after loop stopped"); return nil })
	}()
	d.stop()
	if err := <-result; err == nil {
		t.Fatal("stopped loop accepted job")
	}
}

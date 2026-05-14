package common

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

var (
	ErrLifecycleAlreadyRunning = errors.New("component already running")
	ErrLifecycleNotRunning     = errors.New("component not running")
)

type (
	LifecycleHooks struct {
		Prepare   func(context.Context) (context.Context, context.CancelFunc, error)
		Run       func(context.Context, func()) error
		OnStarted func()
		OnStop    func()
	}

	LifecycleOption func(*LifecycleHooks)

	Lifecycle struct {
		mu     sync.Mutex
		cancel context.CancelFunc
		done   chan struct{}
	}
)

func (l *Lifecycle) Start(ctx context.Context, logger *slog.Logger, hooks LifecycleHooks) error {
	ctx = EnsureContext(ctx)

	if err := ctx.Err(); err != nil {
		if logger != nil {
			logger.Warn("start skipped; context cancelled", "error", err)
		}

		return err
	}

	l.mu.Lock()

	if l.cancel != nil {
		l.mu.Unlock()

		if logger != nil {
			logger.Warn("start skipped; already running")
		}

		return ErrLifecycleAlreadyRunning
	}

	l.mu.Unlock()

	if logger != nil {
		logger.Info("starting")
	}

	runCtx := ctx
	var cancel context.CancelFunc

	if hooks.Prepare != nil {
		var err error
		runCtx, cancel, err = hooks.Prepare(ctx)

		if err != nil {
			if logger != nil {
				logger.Error("failed to start", "error", err)
			}
			return err
		}

		if runCtx == nil {
			runCtx = ctx
		}
	}

	runCtx = EnsureContext(runCtx)

	if cancel == nil {
		runCtx, cancel = context.WithCancel(runCtx)
	}

	done := make(chan struct{})

	l.mu.Lock()
	l.cancel = cancel
	l.done = done
	l.mu.Unlock()

	go l.run(runCtx, logger, hooks, done)
	return nil
}

func (l *Lifecycle) Stop(ctx context.Context, logger *slog.Logger) error {
	ctx = EnsureContext(ctx)

	l.mu.Lock()
	cancel := l.cancel
	done := l.done
	l.mu.Unlock()

	if cancel == nil {
		if logger != nil {
			logger.Warn("stop skipped; not running")
		}

		return ErrLifecycleNotRunning
	}

	if logger != nil {
		logger.Info("stopping")
	}

	cancel()

	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (l *Lifecycle) IsRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancel != nil
}

func (l *Lifecycle) run(ctx context.Context, logger *slog.Logger, hooks LifecycleHooks, done chan struct{}) {
	var started atomic.Bool

	markStarted := func() {
		if started.CompareAndSwap(false, true) {
			if logger != nil {
				logger.Info("started")
			}

			if hooks.OnStarted != nil {
				hooks.OnStarted()
			}
		}
	}

	runErr := error(nil)

	if hooks.Run != nil {
		runErr = hooks.Run(ctx, markStarted)
	} else {
		markStarted()
	}

	if runErr != nil && logger != nil {
		if started.Load() {
			logger.Error("run loop exited with error", "error", runErr)
		} else {
			logger.Error("start failed", "error", runErr)
		}
	}

	if hooks.OnStop != nil {
		hooks.OnStop()
	}

	l.mu.Lock()
	l.cancel = nil
	l.done = nil
	l.mu.Unlock()

	if logger != nil {
		logger.Info("stopped")
	}

	close(done)
}

func EnsureContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}

	return context.Background()
}

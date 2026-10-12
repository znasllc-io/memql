package app

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
)

// Reproduce the observed boot: the scheduler subscribes just AFTER the former
// 30-second wait plus 500ms sleep. The startup event must reach that subscriber
// with the current node address, rather than be lost while the old address stays.
func TestRunStartupWaitsForLateRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newOperatorDrainTestApp(t)
		a.nodeLifecycle.SetObserver(nil)
		a.nodeIdentity.Address = "10.42.3.37:50051"
		a.eventBus = events.NewBus(events.WithLogger(a.Logger))
		scheduler := &readyDep{ready: make(chan struct{})}
		a.Dependencies = []common.Dependency{scheduler}
		done := make(chan struct{})
		stopped := false
		go func() {
			runApplication(RunConfig{
				Logger: a.Logger, DrainDelay: -1, GracePeriod: -1,
				WaitForSignal: func(ctx context.Context) os.Signal { <-ctx.Done(); return nil },
				Stop:          func(context.Context, ...common.Dependency) { stopped = true },
			}, a)
			close(done)
		}()
		defer func() {
			a.RequestOperatorDrain("test cleanup")
			<-done
		}()
		time.Sleep(31 * time.Second)
		synctest.Wait()
		require.Equal(t, node.LifecycleStarting, a.nodeLifecycle.State(), "elapsed time must not mark the node ready")
		require.False(t, stopped)

		observed := make(chan events.Event, 2)
		unsubscribe := a.eventBus.Subscribe(events.TopicSystemStartup, func(e events.Event) { observed <- e })
		defer unsubscribe()
		close(scheduler.ready)
		synctest.Wait()
		require.True(t, a.nodeLifecycle.IsReady())
		select {
		case event := <-observed:
			payload := event.Payload
			require.Equal(t, a.nodeIdentity.Address, payload["node"].(map[string]any)["address"])
		default:
			t.Fatal("the late subscriber missed system.startup")
		}
		require.Empty(t, observed, "startup must be emitted exactly once")
		a.RequestOperatorDrain("finished")
		<-done
		require.True(t, stopped)
		require.Equal(t, node.LifecycleStopped, a.nodeLifecycle.State())
	})
}

func TestRunStartupFailureDoesNotAnnounceReady(t *testing.T) {
	for _, kind := range []string{"operator", "signal", "readiness error"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := newOperatorDrainTestApp(t)
				a.nodeLifecycle.SetObserver(nil)
				a.eventBus = events.NewBus(events.WithLogger(a.Logger))
				var startups atomic.Int32
				unsubscribe := a.eventBus.Subscribe(events.TopicSystemStartup, func(events.Event) { startups.Add(1) })
				defer unsubscribe()
				a.Dependencies = []common.Dependency{&readyDep{ready: make(chan struct{})}}
				signals := make(chan os.Signal, 1)
				waitExited := false
				stopped := 0
				boundedStop := false
				cfg := RunConfig{
					Logger: a.Logger, DrainDelay: -1, GracePeriod: -1,
					WaitForSignal: func(ctx context.Context) os.Signal {
						defer func() { waitExited = true }()
						select {
						case sig := <-signals:
							return sig
						case <-ctx.Done():
							return nil
						}
					},
					Stop: func(ctx context.Context, _ ...common.Dependency) {
						_, boundedStop = ctx.Deadline()
						stopped++
					},
				}
				if kind == "readiness error" {
					cfg.WaitForReady = func(context.Context, []common.Dependency) error { return errors.New("registration failed") }
				}
				done := make(chan struct{})
				go func() { runApplication(cfg, a); close(done) }()
				synctest.Wait()
				switch kind {
				case "operator":
					a.RequestOperatorDrain("cancel boot")
				case "signal":
					signals <- syscall.SIGTERM
				}
				<-done
				require.Zero(t, startups.Load())
				require.Equal(t, 1, stopped)
				require.True(t, boundedStop)
				require.True(t, waitExited, "the signal waiter must not outlive Run")
				require.Equal(t, node.LifecycleStopped, a.nodeLifecycle.State())
			})
		})
	}
}

func TestReadinessCancellationDoesNotBecomeSuccess(t *testing.T) {
	ready := &readyDep{ready: make(chan struct{})}
	close(ready.ready)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, DefaultWaitForDependenciesReady(ctx, []common.Dependency{nil, ready}), context.Canceled)
	require.ErrorIs(t, DefaultWaitForDependenciesReady(ctx, nil), context.Canceled)
}

func TestShutdownWatcherCleanupJoinsSignalWaiter(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	_, _, cleanup := watchShutdownTrigger(func(ctx context.Context) os.Signal {
		close(started)
		<-ctx.Done()
		close(exited)
		return nil
	}, nil)
	<-started
	cleanup()
	select {
	case <-exited:
	default:
		t.Fatal("cleanup returned while the signal waiter was still running")
	}
}

// No scheduling delay is needed: the real signal subscription exists before
// the caller can start dependencies or receive a shutdown during startup.
func TestShutdownWatcherRegistersSignalsBeforeReturning(t *testing.T) {
	ctx, triggers, cleanup := watchShutdownTrigger(nil, nil)
	defer cleanup()
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
	select {
	case trigger := <-triggers:
		require.Equal(t, "signal:terminated", trigger)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("startup shutdown signal was not observed")
	}
}

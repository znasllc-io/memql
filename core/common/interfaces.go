package common

import "context"

type (
	Dependency interface {
		Start(ctx context.Context)
		Stop(ctx context.Context)
		IsRunning() bool
		Order() int
		ComponentName() ComponentName
		// Ready returns a channel that is closed when the component
		// has finished initialization and is ready to accept work.
		// Used for parallel startup coordination.
		Ready() <-chan struct{}
	}
)

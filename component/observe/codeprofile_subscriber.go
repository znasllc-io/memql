package observe

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/visionarys-io/memql/component/events"
	"github.com/visionarys-io/memql/core/common"
)

// CodeProfileSubscriberComponent is the bus-side ComponentName.
const CodeProfileSubscriberComponent = common.ComponentName("observe.codeProfileSubscriber")

// CodeProfileSubscriber bridges v1:observability:codeProfile concept
// events to the in-process observe.profileCache. The flow:
//
//  1. A cockpit user (or seed script, or admin tool) inserts a
//     codeProfile row -- the engine writes it through and emits a
//     graph.node.created._system.v1:observability:codeProfile event
//     (the codeProfile concept is @scope("global"), so events fire
//     under _system regardless of the writer's partition envelope).
//  2. This subscriber matches the pattern, extracts codeReference /
//     level / expiresAt from the event payload, and calls
//     SetProfile so the next observe.Method(fqn) call honors the
//     new level.
//  3. A periodic sweeper (started in Start) drops expired entries
//     so the cache stays small after a "verbose for an hour"
//     window closes.
//
// Lifecycle is a Dependency so the app bootstrap can wire it in
// the engine phase, where the event bus is already alive.
type CodeProfileSubscriber struct {
	logger      *slog.Logger
	bus         *events.Bus
	unsubscribe func()
	stopCh      chan struct{}
	doneCh      chan struct{}
	readyCh     chan struct{}
	running     atomic.Bool
	startOnce   sync.Once
	stopOnce    sync.Once
}

// NewCodeProfileSubscriber constructs the bridge. The events.Bus is
// taken at construction time; the actual Subscribe call runs in
// Start so a not-yet-ready bus surfaces as a startup error rather
// than as a constructor panic.
func NewCodeProfileSubscriber(logger *slog.Logger, bus *events.Bus) *CodeProfileSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	return &CodeProfileSubscriber{
		logger:  logger.With("component", "observe.codeProfileSubscriber"),
		bus:     bus,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		readyCh: make(chan struct{}),
	}
}

// Start subscribes to the codeProfile event pattern and launches the
// expiry sweeper. Idempotent.
func (s *CodeProfileSubscriber) Start(_ context.Context) {
	s.startOnce.Do(func() {
		defer close(s.readyCh)
		if s.bus == nil {
			s.logger.Warn("no event bus available -- skipping codeProfile subscription")
			close(s.doneCh)
			return
		}
		s.running.Store(true)
		// codeProfile is @scope("global"), so its events always
		// fire under the _system partition slot. We pin the pattern
		// to that slot rather than using * so we don't waste a
		// subscriber slot on partition-scoped concept events that
		// can never match.
		s.unsubscribe = s.bus.Subscribe(
			"graph.node.created._system.v1:observability:codeProfile",
			s.handle,
			events.WithSubscriberName("observe.codeProfileSubscriber"),
		)
		go s.runSweeper()
		s.logger.Info("codeProfile subscriber active")
	})
}

// Stop closes the subscription and stops the sweeper.
func (s *CodeProfileSubscriber) Stop(_ context.Context) {
	s.stopOnce.Do(func() {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		close(s.stopCh)
		<-s.doneCh
		s.running.Store(false)
	})
}

// Standard Dependency surface so the app bootstrap can hold this
// alongside the rest of the component graph. Order is chosen to
// match the engine phase's other lightweight subscribers (>5,
// well after database + concept seeder).
func (s *CodeProfileSubscriber) IsRunning() bool             { return s.running.Load() }
func (s *CodeProfileSubscriber) Order() int                  { return 10 }
func (s *CodeProfileSubscriber) ComponentName() common.ComponentName { return CodeProfileSubscriberComponent }
func (s *CodeProfileSubscriber) Ready() <-chan struct{}      { return s.readyCh }

// runSweeper drops expired profile entries on a 1-minute cadence.
// Cheap (single map walk under a write lock) and bounded by however
// many overrides exist; in normal operation we expect a handful.
func (s *CodeProfileSubscriber) runSweeper() {
	defer close(s.doneCh)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n := SweepExpiredProfiles(time.Now())
			if n > 0 {
				s.logger.Debug("swept expired codeProfile entries", "count", n)
			}
		case <-s.stopCh:
			return
		}
	}
}

// handle decodes the event payload and updates the cache. Tolerates
// missing / mistyped fields: log + drop rather than panicking on
// unexpected shapes. The payload follows the standard graph event
// envelope -- the row's payload object is under "payload".
func (s *CodeProfileSubscriber) handle(ev events.Event) {
	payload, ok := ev.Payload["payload"].(map[string]any)
	if !ok {
		s.logger.Debug("codeProfile event missing payload object", "topic", ev.Topic)
		return
	}
	codeReference := stringField(payload, "codeReference")
	if codeReference == "" {
		s.logger.Debug("codeProfile event missing codeReference", "topic", ev.Topic)
		return
	}
	levelStr := stringField(payload, "level")
	level, _ := ParseLevel(levelStr)

	expires := timeField(payload, "expiresAt")
	SetProfile(codeReference, level, expires)
	s.logger.Info("codeProfile applied",
		"codeReference", codeReference,
		"level", level.String(),
		"expiresAt", expires)
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// timeField pulls an RFC3339-ish timestamp from a payload field.
// Returns the zero time when missing / unparseable, which the
// profile cache interprets as "no expiry."
func timeField(m map[string]any, key string) time.Time {
	v, ok := m[key]
	if !ok || v == nil {
		return time.Time{}
	}
	switch x := v.(type) {
	case time.Time:
		return x
	case string:
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t
		}
	}
	return time.Time{}
}

package automations

import (
	"github.com/znasllc-io/memql/component/bus"
)

// SetWiring configures the bus wiring for channel-based communication.
// When set, the scheduler can route engine requests through the bus
// instead of calling engine methods directly.
func (s *Scheduler) SetWiring(w *bus.Wiring) {
	s.wiring = w
}

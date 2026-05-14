package server

import (
	"github.com/visionarys-io/memql/component/bus"
)

// SetWiring configures the bus wiring for channel-based communication.
// When set, HTTP handlers can route engine requests through the bus
// instead of calling engine methods directly.
func (s *Server) SetWiring(w *bus.Wiring) {
	s.wiring = w
}

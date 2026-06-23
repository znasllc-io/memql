package dslspec

import "encoding/json"

// JSON returns the spec serialized as indented JSON -- the portable form a
// cockpit or web/Monaco editor consumes (the gRPC/SDK export endpoint in
// #2125 wraps this). The shape is versioned via Spec.Version; consumers
// should check it against the SpecVersion they were built against.
func (s *Spec) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

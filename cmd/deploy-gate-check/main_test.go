package main

import "testing"

// TestReadyzURLFor pins how the readiness URL is derived from the gRPC addr (or
// an explicit override): http /readyz on :8085 of the addr's host.
func TestReadyzURLFor(t *testing.T) {
	cases := []struct {
		addr, override, want string
	}{
		{"bff:50051", "", "http://bff:8085/readyz"},
		{"cognition-canary:50051", "", "http://cognition-canary:8085/readyz"},
		{"10.0.0.5:50051", "", "http://10.0.0.5:8085/readyz"},
		{"bff:50051", "http://custom/readyz", "http://custom/readyz"}, // override wins
		{"bff", "", "http://bff:8085/readyz"},                         // no port -> host == addr
	}
	for _, c := range cases {
		if got := readyzURLFor(c.addr, c.override); got != c.want {
			t.Errorf("readyzURLFor(%q,%q) = %q, want %q", c.addr, c.override, got, c.want)
		}
	}
}

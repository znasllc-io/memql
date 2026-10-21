//go:build linux

package modelcall

import "testing"

func TestDiscreteHeadroomDoesNotSumDifferentGPUs(t *testing.T) {
	s, ok := nvidiaHeadroom("24564, 20000\n24564, 10000\n")
	if !ok || s.Available != 10000*(1<<20) || s.Total != 24564*(1<<20) {
		t.Fatalf("%+v %t", s, ok)
	}
	for _, bad := range []string{"", "N/A, N/A", "1024, 2048", "0, 0"} {
		if _, ok := nvidiaHeadroom(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

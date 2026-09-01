package main

// The tracking endpoints have a coupling nothing else can check: the front
// door routes what component/server declares, and the mail client fetches what
// component/campaigns' renderer built into the message body. Those are two
// literals in two packages that must be the same string, and they are two
// literals rather than one import because component/server has no other reason
// to depend on component/campaigns -- the same shape UnsubscribePath already
// has, kept the same way.
//
// If they disagree the failure is silent in the worst direction: the front
// door routes a path no mail client ever fetches, the pixel 404s, and every
// campaign reports zero opens forever. Zero opens is a completely plausible
// number, which is what would let it survive a release.
//
// So the agreement is asserted here, in the one package that may import both.

import (
	"testing"

	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/server"
)

func TestTrackingPathsAgreeWithWhatTheRendererBuilds(t *testing.T) {
	// NEGATIVE CONTROL, run by hand rather than asserted: changing
	// campaigns.TrackingOpenPath to "/t/open/" fails this test naming that
	// exact string. A gate that cannot be made to fail is a gate that proves
	// nothing, and this one is a string comparison across a boundary
	// deliberately kept import-free, so there is nothing else holding it.
	declared := server.TrackingPaths()
	if len(declared) == 0 {
		t.Fatal("server.TrackingPaths() is empty; the two endpoints would be served and routed by nothing")
	}

	for _, want := range []struct {
		name string
		path string
	}{
		{"campaigns.TrackingOpenPath", campaigns.TrackingOpenPath},
		{"campaigns.TrackingClickPath", campaigns.TrackingClickPath},
	} {
		found := false
		for _, p := range declared {
			if p == want.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is %q, which server.TrackingPaths() does not declare (%v).\n"+
				"The renderer builds that path into every tracked message; the front door routes "+
				"what TrackingPaths declares. When they disagree the endpoint 404s and every "+
				"campaign reports zero opens -- a plausible number, which is why nothing would "+
				"report it.", want.name, want.path, declared)
		}
	}
}

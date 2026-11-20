package memql

import "testing"

// THE WIRE BETWEEN THE EDGE AND THE BFF FOR THE CALLS SURFACE, spelled once in this
// package and read by both. A change here moves both ends together, so what is left to go
// wrong is the change itself: a rolling deploy has an old edge forwarding to a new bff, or
// the reverse, for as long as the roll takes, and a name that moved under it turns every
// call request into the 404 a stranger gets. So these are pinned as text, and renaming one
// is something a person has to mean.
func TestCallsSurfaceWireNamesArePinned(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"the site stamp":   {CallsStampSiteHeader, "X-Memql-Calls-Site"},
		"the owner stamp":  {CallsStampOwnerHeader, "X-Memql-Calls-Owner"},
		"the base":         {CallsSurfacePathPrefix, "/calls/"},
		"the inbound path": {CallsInboundPath, "/_memql/calls"},
		"the cookie":       {CallsCookieName, "__Host-memql_calls"},
		"account":          {CallsAccountPath, "/calls/account"},
		"unlock":           {CallsUnlockPath, "/calls/unlock"},
		"session":          {CallsSessionPath, "/calls/session"},
		"start":            {CallsStartPath, "/calls/start"},
		"schedule":         {CallsSchedulePath, "/calls/schedule"},
		"reschedule":       {CallsReschedulePath, "/calls/reschedule"},
		"cancel":           {CallsCancelPath, "/calls/cancel"},
		"join":             {CallsJoinPath, "/calls/join"},
		"queue":            {CallsQueuePath, "/calls/queue"},
		"host":             {CallsHostPath, "/calls/host"},
		"leave":            {CallsLeavePath, "/calls/leave"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
	}
	if CallsAccountBodyBytes != 65536 {
		t.Errorf("account body cap = %d, want 64 KiB", CallsAccountBodyBytes)
	}
	if CallsSmallBodyBytes != 1024 {
		t.Errorf("the small body cap = %d, want 1 KiB", CallsSmallBodyBytes)
	}
	if CallsScheduleBodyBytes != 4096 {
		t.Errorf("the schedule body cap = %d, want 4 KiB", CallsScheduleBodyBytes)
	}
}

// NEITHER STAMP IS THE ACCESS SURFACE'S, and neither is the cookie. The edge strips each
// surface's stamps from every request it forwards anywhere else, so a calls request whose
// stamp shared a name with the access surface's would arrive at one handler carrying a
// stamp the edge set for the other.
func TestCallsWireNamesAreNotTheAccessSurfaces(t *testing.T) {
	for _, tc := range []struct{ calls, access string }{
		{CallsStampSiteHeader, AccessStampSiteHeader},
		{CallsStampOwnerHeader, AccessStampOwnerHeader},
		{CallsCookieName, AccessCookieName},
		{CallsSurfacePathPrefix, AccessSurfacePathPrefix},
		{CallsInboundPath, AccessInboundPath},
	} {
		if tc.calls == tc.access {
			t.Errorf("the calls surface and the access surface both use %q", tc.calls)
		}
	}
}

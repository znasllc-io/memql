package http

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// session_created is where the FIRST refresh token is minted, and until
// memql#4327 nothing said so.
//
// Rotation has its own event (session_refreshed, on the activity log since
// memql#4328). The initial mint had none: it happened inside issueSessionForUser
// and was covered only by session_created, which reads as "a session began" and
// is silent about a credential having been issued. Anyone auditing "when was a
// refresh token for this account created" had to know that a session_created
// implies one -- which is exactly the kind of implicit knowledge an audit log
// exists to remove.

type sessionAuditRecorder struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (r *sessionAuditRecorder) Log(_ context.Context, ev identity.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func TestSessionCreatedSaysARefreshTokenWasIssued(t *testing.T) {
	s, _, _ := newRefreshGrantTestServer(t)
	// The shared helper wires the Issuer onto the Rotator only; issuing a
	// session needs it on the Server.
	s.Issuer = s.Rotator.Issuer
	audit := &sessionAuditRecorder{}
	s.Audit = audit

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/oauth/token", nil)
	if _, err := s.issueSessionForUser(rec, req, sessionMintInput{
		UserId:     "v1:identity:user:owner-1",
		IdentityId: "v1:identity:identity:ml-1",
		ClientId:   "portal",
	}); err != nil {
		t.Fatalf("issueSessionForUser: %v", err)
	}

	var created *identity.AuditEvent
	for i := range audit.events {
		if audit.events[i].Action == "session_created" {
			created = &audit.events[i]
		}
	}
	if created == nil {
		t.Fatalf("no session_created event; got %+v", audit.events)
	}
	if created.Stream != identity.StreamAudit {
		t.Errorf("session_created landed on the %q stream; a session beginning is a DECISION and "+
			"stays on the audit log", created.Stream)
	}
	got, ok := created.Detail["refreshTokenIssued"]
	if !ok {
		t.Fatalf("session_created carries no refreshTokenIssued; detail = %v", created.Detail)
	}
	if issued, isBool := got.(bool); !isBool || !issued {
		t.Errorf("refreshTokenIssued = %v (%T), want true", got, got)
	}
}

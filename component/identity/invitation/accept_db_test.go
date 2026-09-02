package invitation_test

// The accept seam against a REAL engine (memql#4880).
//
// component/identity/web/invitation_flow_test.go drives the /invitation pages
// with a fake accept function, and every unit test of the stores drives a fake
// engine. Neither can see what the engine does to a payload on the way to the
// database -- relationship canonicalization, enum checks, the row-authz write
// guard -- which is exactly where the accept path failed in production: the
// enrolment token's `issuedBy` was written as "invitation:<canonical id>", and
// the concept declares that field a parent edge onto v1:identity:user, so the
// canonicalizer refused the wrong-concept tag on every user-invitation accept
// ever attempted. A fake engine has no gates, so a suite full of them was
// green the whole time.
//
// This test is db-gated and boots the real DSL tree, so the three writes the
// accept makes are the three writes production makes.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/adminops"
	"github.com/znasllc-io/memql/component/identity/enrolment"
	"github.com/znasllc-io/memql/component/identity/invitation"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

func TestAcceptAgainstARealEngineHandsOffToEnrolment(t *testing.T) {
	eng, _ := realEngine(t)
	store := &identity.Store{Engine: eng, Logger: discardLogger()}
	enrolments := &enrolment.Store{Engine: eng, Logger: discardLogger()}

	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	inviterId := "v1:identity:user:inviter-" + stamp
	// Lower-case, because the issue path lower-cases the invited address and
	// the account is provisioned from the row, not from what the test typed.
	inviterEmail := strings.ToLower("inviter-" + stamp + "@example.test")
	inviteeEmail := strings.ToLower("invitee-" + stamp + "@example.test")

	ctx := identity.ContextWithSystemCredentialActor(context.Background())
	if err := store.CreateUserOnFirstLogin(ctx, inviterId, "Inviter", inviterEmail, "owner", true, identity.UserProfileSeed{}); err != nil {
		t.Fatalf("create inviter: %v", err)
	}

	// Issued through the SAME admin path the portal uses, so the row under
	// test is the row production writes and not a hand-built approximation.
	svc, err := adminops.New(&adminops.Service{
		Engine:          eng,
		Audit:           &identity.SlogAuditLogger{Logger: discardLogger()},
		Logger:          discardLogger(),
		IdentityBaseURL: func(context.Context) string { return "https://identity.example.test" },
	})
	if err != nil {
		t.Fatalf("adminops.New: %v", err)
	}
	ownerCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId:       inviterId,
		PrimaryEmail: inviterEmail,
		Role:         auth.RoleOwner,
		IdentityId:   "v1:identity:identity:inviter-" + stamp,
	})
	issued := svc.IssueUserInvitation(ownerCtx, adminops.UserInvitation{Email: inviteeEmail, Role: "developer"})
	if !issued.OK {
		t.Fatalf("issue invitation: %s", issued.ErrorMessage)
	}
	plain := tokenFromInvitationURL(t, issued.InvitationURL)

	deps := invitation.AcceptDeps{
		Store:         store,
		Enrolments:    enrolments,
		InternalEmail: func(string) bool { return false },
	}
	out, err := invitation.Accept(ctx, deps, plain, "203.0.113.7")
	if err != nil {
		t.Fatalf("accept refused, which is the production failure -- the invitee sees "+
			"\"we could not finish setting up your account\" with the user row already "+
			"written and the invitation already spent: %v", err)
	}
	if out.EnrolmentCode == "" {
		t.Fatal("accept returned no enrolment code; /enroll would have nothing to redeem")
	}

	t.Run("the enrolment token is attributed to the inviter and names the invitation", func(t *testing.T) {
		row, state, err := enrolments.Resolve(ctx, out.EnrolmentCode, time.Now().UTC())
		if err != nil {
			t.Fatalf("resolve enrolment: %v", err)
		}
		if state != enrolment.StateValid || row == nil {
			t.Fatalf("enrolment state = %q, want %q -- the code the accept handed out does not "+
				"resolve to a live token", state, enrolment.StateValid)
		}
		if row.IssuedBy != inviterId {
			t.Errorf("issuedBy = %q, want the inviter %q: the concept declares issuedBy a parent "+
				"edge onto v1:identity:user, so it has to name the principal whose authority "+
				"minted the token", row.IssuedBy, inviterId)
		}
		if row.InvitationId == "" || row.InvitationId != out.InvitationId ||
			!strings.HasPrefix(row.InvitationId, "v1:identity:invitation:") {
			t.Errorf("invitationId = %q, want %q: an operator reading the enrolment row must "+
				"be able to see it came from an invitation without guessing", row.InvitationId, out.InvitationId)
		}
		if row.UserId != out.UserId || !strings.HasPrefix(row.UserId, "v1:identity:user:") {
			t.Errorf("userId = %q, want the provisioned account %q", row.UserId, out.UserId)
		}
	})

	t.Run("the account carries the invited role", func(t *testing.T) {
		u, err := store.LookupUserByEmail(ctx, inviteeEmail)
		if err != nil || u == nil {
			t.Fatalf("lookup invitee: user=%v err=%v", u, err)
		}
		if u.Role != "developer" {
			t.Errorf("role = %q, want developer", u.Role)
		}
		if u.ID != out.UserId {
			t.Errorf("user id = %q, want %q", u.ID, out.UserId)
		}
	})

	t.Run("the invitation is spent", func(t *testing.T) {
		inv, err := store.LookupInvitationByTokenHash(ctx, invitation.Hash(plain))
		if err != nil || inv == nil {
			t.Fatalf("lookup invitation: row=%v err=%v", inv, err)
		}
		if !strings.EqualFold(inv.Status, "accepted") {
			t.Errorf("status = %q, want accepted", inv.Status)
		}
	})

	t.Run("a second accept of the same token is refused", func(t *testing.T) {
		if _, err := invitation.Accept(ctx, deps, plain, "203.0.113.7"); err == nil {
			t.Fatal("a spent invitation was accepted again, which would mint a second account for one link")
		}
	})
}

// tokenFromInvitationURL reads the plaintext out of the link the issue path
// composes. Both spellings the page accepts are honoured, for the reason
// invitationCodeFrom records.
func tokenFromInvitationURL(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("invitation url %q: %v", link, err)
	}
	for _, key := range []string{"code", "invitation"} {
		if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
			return v
		}
	}
	t.Fatalf("invitation url carries no token: %q", link)
	return ""
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// realEngine boots the real DSL tree against the db-gated database, the same
// shape component/identity/recoverykey's invariant test uses.
func realEngine(t *testing.T) (*memqlengine.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "invitation accept real-engine DB test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = discardLogger()
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng, db
}

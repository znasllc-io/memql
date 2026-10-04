package invitation_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/adminops"
	"github.com/znasllc-io/memql/component/identity/enrolment"
	"github.com/znasllc-io/memql/component/identity/invitation"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type joiningMail struct{ links atomic.Int32 }

func (m *joiningMail) SendSignInDisabledNotice(context.Context, magiclink.NoticeInput) error {
	return nil
}

func (m *joiningMail) SendMagicLink(context.Context, magiclink.SendInput) error {
	m.links.Add(1)
	return nil
}

func TestJoiningPoliciesUseSavedSettingsAcrossReplicas(t *testing.T) {
	writer, db := realEngine(t)
	receiver, err := memqlengine.New(db)
	require.NoError(t, err)
	require.NoError(t, receiver.Init(concept.DefaultRegistry()))
	first := &identity.Store{Engine: writer, Logger: discardLogger(), DirectDB: func() *sql.DB { return db.DB }}
	second := &identity.Store{Engine: receiver, Logger: discardLogger(), DirectDB: func() *sql.DB { return db.DB }}
	ctx := identity.ContextWithSystemCredentialActor(t.Context())
	stamp := fmt.Sprint(time.Now().UnixNano())
	owner := "joining-owner-" + stamp
	require.NoError(t, first.CreateUserOnFirstLogin(ctx, owner, "Owner", owner+"@team.test", "owner", true, identity.UserProfileSeed{}))
	ownerCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleOwner, PrimaryEmail: owner + "@team.test"})
	boot := identity.Config{BaseURL: "https://identity.example.test", RegistrationMode: identity.RegistrationModeOpen, InternalDefaultRole: "owner", InternalDomains: []string{"stale.test"}}
	var deliveries atomic.Int32
	service := func(store *identity.Store) *adminops.Service {
		s, err := adminops.New(&adminops.Service{Engine: store.Engine, AccessRequests: store, Audit: &identity.SlogAuditLogger{Logger: discardLogger()}, Logger: discardLogger(),
			IdentityBaseURL: func(context.Context) string { return boot.BaseURL },
			RegistrationPolicy: func(ctx context.Context) (string, []string, error) {
				cfg, err := store.RegistrationConfig(ctx, boot)
				return string(cfg.RegistrationMode), cfg.RegistrationDomains, err
			},
			SendInvitationEmail: func(context.Context, adminops.InvitationEmail) error { deliveries.Add(1); return nil },
		})
		require.NoError(t, err)
		return s
	}
	adminA, adminB := service(first), service(second)
	require.NoError(t, first.PersistClusterSettings(ctx, identity.ClusterSettingsRow{RegistrationMode: "open", InternalDefaultRole: "writer"}))
	save := func(mode, domains, internal string) {
		res := adminA.UpdateClusterSettings(ownerCtx, adminops.ClusterSettings{RegistrationMode: mode, RegistrationDomains: domains, InternalDomains: internal, InternalDefaultRole: "writer"})
		require.True(t, res.OK, res.ErrorMessage)
	}
	mail := &joiningMail{}
	issuer := &magiclink.Issuer{Cfg: boot, Store: second, Sender: mail, Logger: discardLogger()}
	verifier := &magiclink.Verifier{Cfg: boot, Store: first, Logger: discardLogger()}
	for index, test := range []struct{ mode, host, outcome, role string }{
		{"open", "team.test", "link", "writer"}, {"open", "external.test", "link", "reader"},
		{"domain_restricted", "team.test", "link", "writer"}, {"domain_restricted", "external.test", "refuse", ""},
		{"invite_only", "team.test", "refuse", ""}, {"invite_only", "external.test", "refuse", ""},
		{"waitlist", "team.test", "request", ""}, {"waitlist", "external.test", "request", ""},
	} {
		t.Run(fmt.Sprintf("%s/%s", test.mode, test.host), func(t *testing.T) {
			save(test.mode, "team.test", "team.test")
			email := fmt.Sprintf("joining-%s-%d@%s", stamp, index, test.host)
			before := mail.links.Load()
			issued, err := issuer.Issue(ctx, magiclink.IssueInput{Email: email, AdminSession: true})
			switch test.outcome {
			case "refuse":
				require.ErrorIs(t, err, magiclink.ErrEmailNotAllowed)
				require.Equal(t, before, mail.links.Load())
			case "request":
				require.ErrorIs(t, err, magiclink.ErrAccessRequestPath)
				require.Equal(t, before, mail.links.Load())
			case "link":
				require.NoError(t, err)
				require.NotEmpty(t, issued.RequestId)
				require.Equal(t, before+1, mail.links.Load())
				// Link issued on B, consumed on A, both still holding stale boot config.
				finished, err := verifier.Finish(ctx, magiclink.FinishInput{RequestId: issued.RequestId})
				require.NoError(t, err)
				require.True(t, finished.NewUser)
				user, err := second.LookupUserByEmail(ctx, email)
				require.NoError(t, err)
				require.NotNil(t, user)
				require.Equal(t, test.role, user.Role)
			}
			if test.outcome != "link" {
				user, err := second.LookupUserByEmail(ctx, email)
				require.NoError(t, err)
				require.Nil(t, user)
			}
		})
	}
	// Clearing internal domains must not revive the environment's stale list.
	save("open", "", "")
	policy, err := second.RegistrationConfig(ctx, boot)
	require.NoError(t, err)
	require.False(t, policy.IsInternalEmail("person@stale.test"))
	// Changing admission never blocks a returning account or rewrites its role.
	save("invite_only", "", "")
	returning := fmt.Sprintf("joining-%s-0@team.test", stamp)
	issued, err := issuer.Issue(ctx, magiclink.IssueInput{Email: returning, AdminSession: true, ExistingUser: true})
	require.NoError(t, err)
	_, err = verifier.Finish(ctx, magiclink.FinishInput{RequestId: issued.RequestId})
	require.NoError(t, err)
	user, err := second.LookupUserByEmail(ctx, returning)
	require.NoError(t, err)
	require.Equal(t, "writer", user.Role)

	t.Run("invitation-only admits an invited person and refuses unauthorized review", func(t *testing.T) {
		save("invite_only", "", "team.test")
		address := "invited-" + stamp + "@team.test"
		invited := adminA.IssueUserInvitation(ownerCtx, adminops.UserInvitation{Email: address})
		require.True(t, invited.OK, invited.ErrorMessage)
		policy, err := second.RegistrationConfig(ctx, boot)
		require.NoError(t, err)
		accepted, err := invitation.Accept(ctx, invitation.AcceptDeps{Store: second, Enrolments: &enrolment.Store{Engine: receiver}, InternalEmail: policy.IsInternalEmail, InternalDefaultRole: policy.InternalDefaultRole}, tokenFromInvitationURL(t, invited.InvitationURL), "")
		require.NoError(t, err)
		require.NotEmpty(t, accepted.EnrolmentCode)
		person, err := first.LookupUserByEmail(ctx, address)
		require.NoError(t, err)
		require.Equal(t, "writer", person.Role)
		reader := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: person.ID, Role: auth.RoleReader})
		refused := adminB.ReviewAccessRequest(reader, adminops.AccessRequestReview{RequestID: "any", Decision: "approve"})
		require.Equal(t, int32(adminops.CodePermissionDenied), refused.Code)
		_, err = receiver.Execute(reader, `mutation approveAccessRequest(requestId: "any", reviewedBy: "forged", invitationId: "forged", reviewerNote: "")`)
		require.Error(t, err, "a client cannot forge a review through the underlying DSL")
		deliveries.Store(0)
	})

	t.Run("approval and rejection are shared across replicas", func(t *testing.T) {
		save("waitlist", "", "team.test")
		requestID, address := "review-"+stamp, "review-"+stamp+"@team.test"
		require.NoError(t, first.CreateAccessRequest(ctx, requestID, address, "Applicant", "Join our team", 0, "", "", ""))
		_, err := second.ReadAccessRequest(ctx, requestID)
		require.NoError(t, err) // prime the other node's read
		results := make(chan adminops.Result, 2)
		for _, admin := range []*adminops.Service{adminA, adminB} {
			go func(s *adminops.Service) {
				results <- s.ReviewAccessRequest(ownerCtx, adminops.AccessRequestReview{RequestID: requestID, Decision: "approve", Role: "reader"})
			}(admin)
		}
		one, two := <-results, <-results
		require.NotEqual(t, one.OK, two.OK, "exactly one review must succeed: %+v %+v", one, two)
		winner := one
		if two.OK {
			winner = two
		}
		require.NotEmpty(t, winner.InvitationURL)
		require.Equal(t, int32(1), deliveries.Load())
		request, err := second.ReadAccessRequest(ctx, requestID)
		require.NoError(t, err)
		require.Equal(t, "approved", request.Status)
		policy, err := second.RegistrationConfig(ctx, boot)
		require.NoError(t, err)
		accepted, err := invitation.Accept(ctx, invitation.AcceptDeps{Store: second, Enrolments: &enrolment.Store{Engine: receiver, Logger: discardLogger()}, InternalEmail: policy.IsInternalEmail, InternalDefaultRole: policy.InternalDefaultRole}, tokenFromInvitationURL(t, winner.InvitationURL), "")
		require.NoError(t, err)
		require.NotEmpty(t, accepted.EnrolmentCode)
		created, err := second.LookupUserByEmail(ctx, address)
		require.NoError(t, err)
		require.Equal(t, "reader", created.Role)
		_, err = invitation.Accept(ctx, invitation.AcceptDeps{Store: first, Enrolments: &enrolment.Store{Engine: writer}}, tokenFromInvitationURL(t, winner.InvitationURL), "")
		require.Error(t, err, "an accepted invitation cannot be reused")
		deniedID := "reject-" + stamp
		require.NoError(t, first.CreateAccessRequest(ctx, deniedID, "reject-"+stamp+"@team.test", "", "", 0, "", "", ""))
		require.False(t, adminB.ReviewAccessRequest(ownerCtx, adminops.AccessRequestReview{RequestID: deniedID, Decision: "reject"}).OK)
		rejected := adminB.ReviewAccessRequest(ownerCtx, adminops.AccessRequestReview{RequestID: deniedID, Decision: "reject", Note: "No available access"})
		require.True(t, rejected.OK, rejected.ErrorMessage)
		require.Equal(t, int32(1), deliveries.Load())
		request, err = first.ReadAccessRequest(ctx, deniedID)
		require.NoError(t, err)
		require.Equal(t, "rejected", request.Status)
		require.False(t, adminA.ReviewAccessRequest(ownerCtx, adminops.AccessRequestReview{RequestID: deniedID, Decision: "approve"}).OK)
	})

	t.Run("notification throttling and failure retry cross replicas", func(t *testing.T) {
		var recipients []string
		require.NoError(t, first.NotifyAccessRequests(ctx, boot, func(_ context.Context, address string) error {
			recipients = append(recipients, address)
			return nil
		}))
		require.Contains(t, recipients, owner+"@team.test", "empty notification list must reach the cluster owner")
		cfg := boot
		cfg.AccessRequestNotifyEmails = []string{"notices-" + stamp + "@team.test"}
		cfg.AccessRequestNotifyThrottle = time.Minute
		var sent atomic.Int32
		var wg sync.WaitGroup
		for _, store := range []*identity.Store{first, second} {
			wg.Add(1)
			go func(s *identity.Store) {
				defer wg.Done()
				require.NoError(t, s.NotifyAccessRequests(ctx, cfg, func(context.Context, string) error { sent.Add(1); return nil }))
			}(store)
		}
		wg.Wait()
		require.Equal(t, int32(1), sent.Load())
		cfg.AccessRequestNotifyEmails = []string{"retry-" + stamp + "@team.test"}
		require.Error(t, first.NotifyAccessRequests(ctx, cfg, func(context.Context, string) error { return errors.New("mail offline") }))
		require.NoError(t, second.NotifyAccessRequests(ctx, cfg, func(_ context.Context, address string) error {
			require.True(t, strings.HasPrefix(address, "retry-"))
			sent.Add(1)
			return nil
		}))
		require.Equal(t, int32(2), sent.Load())
	})
}

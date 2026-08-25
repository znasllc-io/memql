package emailsender

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/integrations/email"
)

// invitation_test.go -- the invitation email's copy and its no-sender
// behaviour (memql#4584).
//
// The bodies are asserted through the pure builders rather than through a
// wired integration, because what is under test is what the message SAYS. The
// transport is SendMagicLink's, unchanged and already covered by
// no_sender_test.go; duplicating that here would test the same code twice and
// the copy not at all.

func sampleInvite() UserInvitationInput {
	return UserInvitationInput{
		Email:       "invitee@acme.com",
		InviterName: "Jo Owner",
		Role:        "admin",
		LinkURL:     "https://identity.acme.com/login?invitation=mql_inv_abc",
		ExpiresAt:   time.Date(2026, 9, 1, 20, 19, 0, 0, time.UTC),
		BrandName:   "MemQL",
	}
}

// The three facts the invitation has to carry. A recipient who cannot tell who
// invited them, to what, and how long they have is being asked to click a
// bearer link from a stranger.
func TestInvitationBodiesStateInviterRoleAndExpiry(t *testing.T) {
	in := sampleInvite()
	for _, body := range []struct{ name, text string }{
		{"text", buildInvitationText(in)},
		{"html", buildInvitationHTML(in)},
	} {
		t.Run(body.name, func(t *testing.T) {
			for _, want := range []string{
				"Jo Owner",         // who invited them
				"admin",            // to what role
				"1 September 2026", // until when
				in.LinkURL,         // and the way in
			} {
				if !strings.Contains(body.text, want) {
					t.Errorf("body does not mention %q:\n%s", want, body.text)
				}
			}
		})
	}
}

// An absent inviter or role must degrade to honest copy, never to a blank
// where a name should be or a role the server never promised.
func TestInvitationCopyDegradesWithoutInviterOrRole(t *testing.T) {
	in := sampleInvite()
	in.InviterName = ""
	in.Role = ""
	text := buildInvitationText(in)

	if strings.Contains(text, "  ") || strings.Contains(text, " has invited") {
		t.Errorf("an empty inviter left a gap in the copy:\n%s", text)
	}
	if !strings.Contains(text, "You have been invited to join MemQL.") {
		t.Errorf("missing the unattributed form:\n%s", text)
	}
	if !strings.Contains(text, "default role") {
		t.Errorf("an empty role must say the cluster default, not name one:\n%s", text)
	}
}

// The same refusal SendMagicLink gives, because it is literally the same
// function (memql#4477). An install that must deliver mail must not be told an
// invitation was sent when no sender exists to send it.
func TestSendUserInvitationRefusesWithNoSenderOnACloudInstall(t *testing.T) {
	cloudInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "https://identity.acme.com"})

	err := s.SendUserInvitation(context.Background(), sampleInvite())
	if !errors.Is(err, email.ErrLogOnlyRefused) {
		t.Fatalf("SendUserInvitation = %v, want ErrLogOnlyRefused", err)
	}
}

// ...and the same log-only allowance locally, where the log is the inbox and a
// developer has to be able to copy the link out of it.
func TestSendUserInvitationStillLogsWithNoSenderLocally(t *testing.T) {
	localInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "http://identity.memql.localhost"})

	if err := s.SendUserInvitation(context.Background(), sampleInvite()); err != nil {
		t.Fatalf("a local install must keep logging the invitation: %v", err)
	}
}

// An invitation with no link is worse than no invitation: it tells somebody
// they have been invited and gives them no way in, so they wait.
func TestSendUserInvitationRefusesWithoutALink(t *testing.T) {
	localInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "http://identity.memql.localhost"})

	in := sampleInvite()
	in.LinkURL = ""
	if err := s.SendUserInvitation(context.Background(), in); err == nil {
		t.Fatal("a linkless invitation was accepted for sending")
	}
}

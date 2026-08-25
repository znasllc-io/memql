package emailsender

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	"github.com/znasllc-io/memql/integrations/email"
)

// no_sender_test.go -- the last of the layers that reported success
// (memql#4477).
//
// This shim resolves the email integration off the engine's registry at call
// time, and when it finds nothing it used to log the magic link and return
// nil. That is the right answer on a developer's laptop, where the log IS the
// inbox. On an install that must really deliver mail it is the same
// fails-upward defect one layer further out: the issuer records the send as
// having worked, and the person waiting on the link is the only evidence.
//
// A nil Engine is exactly the state under test -- resolveSender returns nil
// when the registry has no email provider, which is what a build without the
// plug-in, or a factory that opted out, produces.

func cloudInstall(t *testing.T) {
	t.Helper()
	t.Setenv(email.DomainEnv, "acme.com")
	t.Setenv(email.AllowLogOnlyEnv, "")
}

func localInstall(t *testing.T) {
	t.Helper()
	t.Setenv(email.DomainEnv, "memql.localhost")
	t.Setenv(email.AllowLogOnlyEnv, "")
}

func TestSendMagicLinkRefusesWithNoSenderOnACloudInstall(t *testing.T) {
	cloudInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "https://identity.acme.com"})

	err := s.SendMagicLink(context.Background(), magiclink.SendInput{
		Email:     "owner@acme.com",
		LinkURL:   "https://identity.acme.com/auth/complete?token=x",
		BrandName: "MemQL",
		ExpiresIn: 10 * time.Minute,
		Bootstrap: true,
	})
	if !errors.Is(err, email.ErrLogOnlyRefused) {
		t.Fatalf("SendMagicLink = %v, want ErrLogOnlyRefused", err)
	}
}

func TestSendMagicLinkStillLogsWithNoSenderLocally(t *testing.T) {
	localInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "http://identity.memql.localhost"})

	if err := s.SendMagicLink(context.Background(), magiclink.SendInput{
		Email:     "dev@memql.localhost",
		LinkURL:   "http://identity.memql.localhost/auth/complete?token=x",
		BrandName: "MemQL",
		ExpiresIn: 10 * time.Minute,
	}); err != nil {
		t.Fatalf("a local install must keep logging the link: %v", err)
	}
}

func TestSignInNoticesRefuseWithNoSenderOnACloudInstall(t *testing.T) {
	cloudInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "https://identity.acme.com"})

	if err := s.SendSignInDisabledNotice(context.Background(), magiclink.NoticeInput{
		Email:     "owner@acme.com",
		BrandName: "MemQL",
	}); !errors.Is(err, email.ErrLogOnlyRefused) {
		t.Errorf("SendSignInDisabledNotice = %v, want ErrLogOnlyRefused", err)
	}

	if err := s.SendNewSignIn(context.Background(), identity.SignInNotice{
		Email:     "owner@acme.com",
		BrandName: "MemQL",
		SessionId: "v1:identity:authSession:s1",
		UserId:    "v1:identity:user:u1",
	}); !errors.Is(err, email.ErrLogOnlyRefused) {
		t.Errorf("SendNewSignIn = %v, want ErrLogOnlyRefused", err)
	}
}

// TestSignInNoticesStillLogLocally is the negative control for the pair above.
func TestSignInNoticesStillLogLocally(t *testing.T) {
	localInstall(t)
	s := New(nil, nil, identity.Config{BaseURL: "http://identity.memql.localhost"})

	if err := s.SendSignInDisabledNotice(context.Background(), magiclink.NoticeInput{
		Email:     "dev@memql.localhost",
		BrandName: "MemQL",
	}); err != nil {
		t.Errorf("SendSignInDisabledNotice: %v", err)
	}
	if err := s.SendNewSignIn(context.Background(), identity.SignInNotice{
		Email:     "dev@memql.localhost",
		BrandName: "MemQL",
		SessionId: "v1:identity:authSession:s1",
		UserId:    "v1:identity:user:u1",
	}); err != nil {
		t.Errorf("SendNewSignIn: %v", err)
	}
}

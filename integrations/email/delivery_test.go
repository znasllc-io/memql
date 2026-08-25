package email

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// clearMailEnv blanks every variable either lane reads, so a developer
// machine that really does have mail credentials exported cannot turn a
// refusal test green by accident. Blank rather than unset because that is
// what the resolver treats as absent, and t.Setenv restores whatever was
// there when the test ends.
func clearMailEnv(t *testing.T) {
	t.Helper()
	g, l := DefaultGraphEnvKeys(), LegacyGraphEnvKeys()
	s := DefaultEnvKeys()
	for _, k := range []string{
		g.TenantId, g.ClientId, g.ClientSecret, g.SenderAddr, g.FromName,
		l.TenantId, l.ClientId, l.ClientSecret, l.SenderAddr, l.FromName,
		s.Host, s.Port, s.Username, s.Password, s.FromAddr, s.FromName,
		DomainEnv, AllowLogOnlyEnv,
	} {
		if k != "" {
			t.Setenv(k, "")
		}
	}
}

func TestIsLocalDomain(t *testing.T) {
	local := []string{
		"memql.localhost", // the local overlay's committed default
		"portal.memql.localhost",
		"localhost",
		"127.0.0.1",
		"::1",
		"0.0.0.0",
		"lab.local.acme.com", // the *.local.<domain> dev wildcard
		"",                   // no domain configured at all
	}
	for _, d := range local {
		if !IsLocalDomain(d) {
			t.Errorf("IsLocalDomain(%q) = false, want true", d)
		}
	}

	remote := []string{
		"acme.com",
		"memql.acme.com",
		"lab.example.com",    // `make up DOMAIN=lab.example.com` -- a real name
		"localhost.acme.com", // "localhost" as a LABEL is not the .localhost TLD
		"notlocal.acme.com",  // second label must be exactly "local"
		"acme.localhost.com", // ...and .localhost must be the TLD, not a middle
	}
	for _, d := range remote {
		if IsLocalDomain(d) {
			t.Errorf("IsLocalDomain(%q) = true, want false", d)
		}
	}
}

func TestDeliveryRequired(t *testing.T) {
	cases := []struct {
		name         string
		domain       string
		allowLogOnly string
		want         bool
	}{
		{name: "cloud domain requires delivery", domain: "acme.com", want: true},
		{name: "custom local cluster domain requires delivery", domain: "lab.example.com", want: true},
		{name: "dot-localhost does not", domain: "memql.localhost", want: false},
		{name: "unset domain does not", domain: "", want: false},
		{name: "break-glass opt-out", domain: "acme.com", allowLogOnly: "true", want: false},
		{name: "break-glass explicitly off", domain: "acme.com", allowLogOnly: "false", want: true},
		{name: "break-glass garbage is not an opt-out", domain: "acme.com", allowLogOnly: "yes please", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearMailEnv(t)
			t.Setenv(DomainEnv, tc.domain)
			t.Setenv(AllowLogOnlyEnv, tc.allowLogOnly)
			if got := DeliveryRequired(); got != tc.want {
				t.Fatalf("DeliveryRequired() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRefuseLogOnlyNamesTheWayOut: the whole point of refusing is that the
// operator learns what to set. An error that only says "no" reproduces the
// silence it replaced.
func TestRefuseLogOnlyNamesTheWayOut(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")

	err := RefuseLogOnly("boot")
	if err == nil {
		t.Fatal("RefuseLogOnly returned nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"acme.com",
		"MEMQL_EMAIL_AZURE_TENANT_ID",
		"MEMQL_EMAIL_AZURE_CLIENT_ID",
		"MEMQL_EMAIL_AZURE_CLIENT_SECRET",
		"MEMQL_EMAIL_SENDER",
		"SMTP_HOST",
		AllowLogOnlyEnv,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not mention %q\ngot: %s", want, msg)
		}
	}
	if !errors.Is(err, ErrLogOnlyRefused) {
		t.Error("refusal does not wrap ErrLogOnlyRefused")
	}
}

// TestNewSenderFromEnvRefusesOnCloudDomain is the boot gate: with no
// credentials on a real domain the factory must hand back an error, which
// app.materializePlugins turns into a fatal. Before this the same input
// produced a LogSender and a cluster that ran for days telling everyone it
// had sent mail.
func TestNewSenderFromEnvRefusesOnCloudDomain(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")

	sender, err := NewSenderFromEnv("", nil)
	if err == nil {
		t.Fatalf("NewSenderFromEnv returned no error; got sender %T", sender)
	}
	if !errors.Is(err, ErrLogOnlyRefused) {
		t.Fatalf("error does not wrap ErrLogOnlyRefused: %v", err)
	}
	if sender != nil {
		t.Errorf("a refused selection must not also return a Sender; got %T", sender)
	}
}

// TestNewSenderFromEnvRefusesHalfConfiguredSMTP covers the second fall-through:
// SMTP_HOST set, SMTP_FROM_ADDR missing. It used to Warn and return a
// LogSender, which is the same silent-success failure by a different route.
func TestNewSenderFromEnvRefusesHalfConfiguredSMTP(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")
	t.Setenv(DefaultEnvKeys().Host, "smtp.example.com")

	if _, err := NewSenderFromEnv("", nil); !errors.Is(err, ErrLogOnlyRefused) {
		t.Fatalf("half-configured SMTP on a cloud domain: err = %v, want ErrLogOnlyRefused", err)
	}
}

func TestNewSenderFromEnvKeepsLogSenderLocally(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "memql.localhost")

	sender, err := NewSenderFromEnv("", nil)
	if err != nil {
		t.Fatalf("local install must still get a LogSender: %v", err)
	}
	if _, ok := sender.(*LogSender); !ok {
		t.Fatalf("got %T, want *LogSender", sender)
	}
}

func TestNewSenderFromEnvBreakGlassKeepsLogSender(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")
	t.Setenv(AllowLogOnlyEnv, "true")

	sender, err := NewSenderFromEnv("", nil)
	if err != nil {
		t.Fatalf("%s=true must permit log-only: %v", AllowLogOnlyEnv, err)
	}
	if _, ok := sender.(*LogSender); !ok {
		t.Fatalf("got %T, want *LogSender", sender)
	}
}

// TestNewSenderFromEnvStillSelectsGraph is the negative control for every
// refusal above: with credentials present the gate must be invisible. Without
// this, a bug that refused unconditionally would pass the whole file.
func TestNewSenderFromEnvStillSelectsGraph(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")
	g := DefaultGraphEnvKeys()
	t.Setenv(g.TenantId, "tenant-id")
	t.Setenv(g.ClientId, "client-id")
	t.Setenv(g.ClientSecret, "client-secret")
	t.Setenv(g.SenderAddr, "noreply@acme.com")

	sender, err := NewSenderFromEnv("", nil)
	if err != nil {
		t.Fatalf("fully configured Graph must not be refused: %v", err)
	}
	if _, ok := sender.(*GraphSender); !ok {
		t.Fatalf("got %T, want *GraphSender", sender)
	}
}

// TestLogSenderRefusesSendWhenDeliveryRequired is the belt to the boot gate's
// braces, and it is not redundant: component/grpc/guest_handlers.go builds a
// LogSender directly, three times, on paths the plug-in factory never touches.
// A guard that lived only in NewSenderFromEnv would leave those reporting
// success forever.
func TestLogSenderRefusesSendWhenDeliveryRequired(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")

	err := NewLogSender(nil).Send(context.Background(), Message{
		To:       "owner@acme.com",
		Subject:  "Claim ownership of MemQL",
		TextBody: "link",
	})
	if !errors.Is(err, ErrLogOnlyRefused) {
		t.Fatalf("LogSender.Send = %v, want ErrLogOnlyRefused", err)
	}

	// A campaign drain must not queue this forever: nothing about waiting
	// makes an unconfigured sender configured.
	var se *SendError
	if !errors.As(err, &se) || !se.Permanent {
		t.Errorf("refusal must be a permanent SendError; got %#v", se)
	}
}

func TestLogSenderStillLogsLocally(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "memql.localhost")

	err := NewLogSender(nil).Send(context.Background(), Message{
		To:       "dev@memql.localhost",
		Subject:  "Sign in to MemQL",
		TextBody: "link",
	})
	if err != nil {
		t.Fatalf("local log-only send must succeed: %v", err)
	}
}

// TestLogSenderDecidesAtConstruction pins the timing. The predicate is read
// when the Sender is built, not on every Send: a process's domain does not
// change under it, and re-reading per send would make the refusal depend on
// whatever the last test left in the environment.
func TestLogSenderDecidesAtConstruction(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")
	refusing := NewLogSender(nil)

	t.Setenv(DomainEnv, "memql.localhost")
	if err := refusing.Send(context.Background(), Message{
		To: "owner@acme.com", Subject: "s", TextBody: "b",
	}); !errors.Is(err, ErrLogOnlyRefused) {
		t.Fatalf("a Sender built on a cloud domain must keep refusing; got %v", err)
	}
}

// TestLazySenderBaselineRefuses closes the runtime half. LazySender exists so
// credentials seeded after boot are picked up on first Send; when nothing
// resolves there either, the baseline it falls back to must refuse rather than
// return nil.
func TestLazySenderBaselineRefuses(t *testing.T) {
	clearMailEnv(t)
	t.Setenv(DomainEnv, "acme.com")

	empty := func(context.Context, string) (string, error) { return "", nil }
	lazy := NewLazySender(NewLogSender(nil), empty, empty, nil)

	err := lazy.Send(context.Background(), Message{
		To: "owner@acme.com", Subject: "s", TextBody: "b",
	})
	if !errors.Is(err, ErrLogOnlyRefused) {
		t.Fatalf("LazySender.Send = %v, want ErrLogOnlyRefused", err)
	}
}

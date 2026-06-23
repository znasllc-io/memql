package email

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// stubSender records the messages Send was called with, useful for
// asserting that the lazy resolver picked the right concrete Sender.
type stubSender struct {
	id    string
	count atomic.Int32
}

func (s *stubSender) Send(_ context.Context, _ Message) error {
	s.count.Add(1)
	return nil
}

func TestLazySender_PrefersEnvResolvedWhenNotLogSender(t *testing.T) {
	envSender := &stubSender{id: "env"}
	resolveCalled := false

	lazy := NewLazySender(
		envSender,
		func(_ context.Context, _ string) (string, error) {
			resolveCalled = true
			return "should-not-be-used", nil
		},
		nil,
		nil,
	)

	msg := Message{To: "a@b.c", Subject: "s", TextBody: "t"}
	if err := lazy.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if envSender.count.Load() != 1 {
		t.Fatalf("expected env sender to receive the call, got count=%d", envSender.count.Load())
	}
	if resolveCalled {
		t.Fatalf("memql resolver should not be consulted when env Sender is non-Log")
	}
}

func TestLazySender_FallsThroughToMemqlGraph(t *testing.T) {
	// env resolves to LogSender (no env config) -- lazy must consult memql.
	envSender := NewLogSender(nil)

	vars := map[string]string{
		"MEMQL_EMAIL_AZURE_TENANT_ID": "tenant-1",
		"MEMQL_EMAIL_AZURE_CLIENT_ID": "client-1",
		"MEMQL_EMAIL_SENDER":          "noreply@dev.local",
		"MEMQL_EMAIL_FROM_NAME":       "memQL Dev",
	}
	secrets := map[string]string{
		"MEMQL_EMAIL_AZURE_CLIENT_SECRET": "sekret",
	}

	lazy := NewLazySender(
		envSender,
		func(_ context.Context, name string) (string, error) {
			if v, ok := vars[name]; ok {
				return v, nil
			}
			return "", nil
		},
		func(_ context.Context, name string) (string, error) {
			if v, ok := secrets[name]; ok {
				return v, nil
			}
			return "", nil
		},
		nil,
	)

	// First Send triggers resolution. We can't actually deliver, but
	// we can assert the resolved Sender is a *GraphSender by reaching
	// in once.
	msg := Message{To: "a@b.c", Subject: "s", TextBody: "t"}
	// Let resolve run by manually calling once-protected path; we
	// don't want a real Graph round-trip, so peek at internal state
	// after one Send attempt with a context-cancelled to bail early.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // GraphSender.Send will fail fast on this ctx, that's fine

	_ = lazy.Send(ctx, msg) // first call resolves + delegates

	if _, ok := lazy.resolved.(*GraphSender); !ok {
		t.Fatalf("expected resolved Sender to be *GraphSender, got %T", lazy.resolved)
	}
}

func TestLazySender_FallsThroughToMemqlSMTPWhenGraphIncomplete(t *testing.T) {
	envSender := NewLogSender(nil)

	vars := map[string]string{
		// Graph deliberately incomplete (missing MEMQL_EMAIL_SENDER).
		"MEMQL_EMAIL_AZURE_TENANT_ID": "tenant-1",
		// SMTP path fully set.
		"SMTP_HOST":      "smtp.example.com",
		"SMTP_PORT":      "2525",
		"SMTP_FROM_ADDR": "no-reply@example.com",
		"SMTP_USERNAME":  "smtp-user",
	}
	secrets := map[string]string{
		"SMTP_PASSWORD": "smtp-secret",
	}

	lazy := NewLazySender(
		envSender,
		func(_ context.Context, name string) (string, error) { return vars[name], nil },
		func(_ context.Context, name string) (string, error) { return secrets[name], nil },
		nil,
	)

	_ = lazy.Send(context.Background(), Message{To: "a@b.c", Subject: "s", TextBody: "t"})

	if _, ok := lazy.resolved.(*SMTPSender); !ok {
		t.Fatalf("expected resolved Sender to be *SMTPSender, got %T", lazy.resolved)
	}
}

func TestLazySender_FallsBackToLogWhenNothingConfigured(t *testing.T) {
	envSender := NewLogSender(nil)
	lazy := NewLazySender(
		envSender,
		func(_ context.Context, _ string) (string, error) { return "", nil },
		func(_ context.Context, _ string) (string, error) { return "", nil },
		nil,
	)

	msg := Message{To: "a@b.c", Subject: "s", TextBody: "t"}
	if err := lazy.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := lazy.resolved.(*LogSender); !ok {
		t.Fatalf("expected resolved Sender to be *LogSender, got %T", lazy.resolved)
	}
}

func TestLazySender_ResolvesOnlyOnce(t *testing.T) {
	envSender := NewLogSender(nil)
	var calls atomic.Int32

	lazy := NewLazySender(
		envSender,
		func(_ context.Context, _ string) (string, error) {
			calls.Add(1)
			return "", nil
		},
		nil,
		nil,
	)

	msg := Message{To: "a@b.c", Subject: "s", TextBody: "t"}
	for i := 0; i < 5; i++ {
		_ = lazy.Send(context.Background(), msg)
	}

	// Each resolve attempt looks up several keys; what matters is
	// that the once-gate prevents a SECOND batch on the next Send.
	first := calls.Load()
	if first == 0 {
		t.Fatalf("resolver was never consulted")
	}
	for i := 0; i < 5; i++ {
		_ = lazy.Send(context.Background(), msg)
	}
	if calls.Load() != first {
		t.Fatalf("resolver was consulted again after the first Send (first=%d, total=%d)", first, calls.Load())
	}
}

func TestLazySender_ResolverErrorTreatedAsMissing(t *testing.T) {
	envSender := NewLogSender(nil)
	lazy := NewLazySender(
		envSender,
		func(_ context.Context, _ string) (string, error) { return "", errors.New("db error") },
		func(_ context.Context, _ string) (string, error) { return "", errors.New("db error") },
		nil,
	)

	msg := Message{To: "a@b.c", Subject: "s", TextBody: "t"}
	if err := lazy.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := lazy.resolved.(*LogSender); !ok {
		t.Fatalf("on resolver error, expected *LogSender fallback, got %T", lazy.resolved)
	}
}

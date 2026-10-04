package email

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/secret"
)

type captureStore struct {
	sealed string
	fail   bool
	calls  int
}

func (s *captureStore) Execute(ctx context.Context, query string) (any, error) {
	s.calls++
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || !ac.Synthetic || !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("capture storage must use the internal service actor")
	}
	if s.fail {
		return nil, errors.New("sensitive query: " + query)
	}
	if strings.HasPrefix(query, "mutation recordCapturedEmail") {
		match := regexp.MustCompile(`encryptedValue: ("[^"\\]*(?:\\.[^"\\]*)*")`).FindStringSubmatch(query)
		if len(match) != 2 {
			return nil, errors.New("missing ciphertext")
		}
		value, err := strconv.Unquote(match[1])
		s.sealed = value
		return nil, err
	}
	if !strings.HasPrefix(query, "query capturedEmails") || !strings.Contains(query, "since:") {
		return nil, errors.New("unbounded inbox read")
	}
	return []map[string]any{{"encryptedValue": s.sealed}}, nil
}

func TestCapturePersistsEncryptedMailAndAnotherReplicaReadsIt(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	store := &captureStore{}
	a := &CaptureSender{store: store}
	msg := Message{To: "recipient@example.test", Subject: "Sign in", TextBody: "https://identity.example.test/complete?token=secret-token", HTMLBody: "<p>Hello</p>"}
	if err := a.Send(context.Background(), msg, SendAs{AccountID: "client", Address: "news@client.test", FromName: "Client"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(store.sealed, "secret-token") || strings.Contains(store.sealed, msg.To) {
		t.Fatal("plaintext mail was stored")
	}
	plain, err := secret.Decrypt(store.sealed)
	if err != nil || !strings.Contains(plain, msg.TextBody) {
		t.Fatalf("persisted mail did not round trip: %v", err)
	}
	// This integration has no sender-local state from replica A.
	b := NewIntegration(&CaptureSender{store: store}, nil)
	rows, err := b.handleInbox(configureCtx(auth.RoleDeveloper), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(rows)
	if !strings.Contains(string(encoded), "secret-token") || !strings.Contains(string(encoded), "news@client.test") || !strings.Contains(string(encoded), `\"accountId\":\"client\"`) && !strings.Contains(string(encoded), `"accountId":"client"`) {
		t.Fatalf("receiving replica lost content or organization: %s", encoded)
	}
}

func TestCaptureInboxRefusesNonOperatorsBeforeReading(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), configureCtx(auth.RoleAdmin), configureCtx(auth.RoleWriter), configureCtx(auth.RoleReader), auth.ContextWithAccess(context.Background(), auth.SystemActor("test"))} {
		store := &captureStore{}
		i := NewIntegration(&CaptureSender{store: store}, nil)
		if _, err := i.handleInbox(ctx, nil, 0); err == nil {
			t.Fatal("unauthorized inbox read")
		}
		if store.calls != 0 {
			t.Fatal("refused caller reached message storage")
		}
	}
}

func TestCaptureFailureNeverReportsDeliveryOrLeaksBody(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	msg := Message{To: "a@example.test", Subject: "Hello", TextBody: "private-token"}
	for _, s := range []*CaptureSender{{}, {store: &captureStore{fail: true}}} {
		err := s.Send(context.Background(), msg, SendAs{})
		if err == nil || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "encryptedValue") {
			t.Fatalf("unsafe capture failure: %v", err)
		}
	}
	store := &captureStore{}
	msg.HTMLBody = strings.Repeat("x", maxCaptureBytes+1)
	if err := (&CaptureSender{store: store}).Send(context.Background(), msg, SendAs{}); err == nil || store.calls != 0 {
		t.Fatal("unbounded body accepted")
	}
}

func TestCaptureTransportCannotFallThroughToExternalMail(t *testing.T) {
	t.Setenv(TransportEnv, "capture")
	t.Setenv("MEMQL_EMAIL_SMTP_HOST", "smtp.example.test")
	sender, err := NewSenderFromEnv("", nil)
	if err != nil {
		t.Fatal(err)
	}
	lazy := NewLazySender(sender, nil, nil, nil)
	if !CapturesMessages(lazy) || !CapturesMessages(sender) {
		t.Fatal("capture did not override external configuration")
	}
	if CapturesMessages(nil) || CapturesMessages(NewLazySender(nil, nil, nil, nil)) {
		t.Fatal("unconfigured external sender reported capture")
	}
	t.Setenv(TransportEnv, "misspelled")
	if _, err := NewSenderFromEnv("", nil); err == nil {
		t.Fatal("unknown transport silently selected external mail")
	}
}

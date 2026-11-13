package reviewspack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Unit tests inject only coordination, never substitute production encryption.
// Real PostgreSQL gates and mutation fences are covered by the database tests.
type fakeRequestLease struct{ err error }

func (f *fakeRequestLease) Context(ctx context.Context) context.Context { return ctx }
func (f *fakeRequestLease) Verify(context.Context) error                { return f.err }
func (f *fakeRequestLease) Release()                                    {}
func fakeRequestCoordinator(context.Context, string) (requestLease, error) {
	return &fakeRequestLease{}, nil
}

func TestRetryKeepsAcceptedTokenAfterFinalWriteFails(t *testing.T) {
	r, w, m, p := dueFixture(t)
	persist := w.onWrite
	w.onWrite = func(ctx context.Context, mutation string, row map[string]any) error {
		if row["sendState"] == "sent" {
			return errors.New("lost final write")
		}
		return persist(ctx, mutation, row)
	}
	first, err := p.sendDueRequests(fromAutomation(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || !strings.Contains(string(first[0].Payload), `"failed":1`) {
		t.Fatal("first send or final-write failure not observed")
	}
	id := asString(m.calls[0]["requestId"])
	before := requestWrite(r.requests[id], id)
	token := asString(m.calls[0]["fields"].(map[string]any)["reviewToken"])
	if asString(before["tokenCiphertext"]) == "" || strings.Contains(asString(before["tokenCiphertext"]), token) {
		t.Fatal("retry bearer was not encrypted")
	}
	// Campaigns returns its accepted receipt for the same request. The fake
	// captures the retry's fields so the original delivered bearer is proved.
	w.onWrite = persist
	p.now = func() time.Time { return dueNow.Add(24 * time.Hour) }
	if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 || m.calls[1]["fields"].(map[string]any)["reviewToken"] != token {
		t.Fatal("retry replaced the delivered token")
	}
	after := r.requests[id]
	for _, key := range []string{"tokenHash", "issuedAt", "expiresAt"} {
		if before[key] != after[key] {
			t.Fatalf("retry changed %s", key)
		}
	}
	if after["sendState"] != "sent" || after["tokenCiphertext"] != "" {
		t.Fatal("accepted result did not settle the retry envelope")
	}
}

func TestUncertainAndFailedRetriesKeepOriginalExpiryAndBearer(t *testing.T) {
	for _, state := range []string{"pending", "failed"} {
		t.Run(state, func(t *testing.T) {
			r, _, m, p := dueFixture(t)
			m.answer = state
			if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
				t.Fatal(err)
			}
			id := asString(m.calls[0]["requestId"])
			original := requestWrite(r.requests[id], id)
			token := m.calls[0]["fields"].(map[string]any)["reviewToken"]
			p.now = func() time.Time { return dueNow.Add(48 * time.Hour) }
			if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
				t.Fatal(err)
			}
			if len(m.calls) != 2 || m.calls[1]["fields"].(map[string]any)["reviewToken"] != token {
				t.Fatal("retry minted another bearer")
			}
			for _, key := range []string{"issuedAt", "expiresAt", "tokenHash", "tokenCiphertext"} {
				if original[key] != r.requests[id][key] {
					t.Fatalf("retry changed %s", key)
				}
			}
		})
	}
}

func TestUnsafeRetriesLeaveOriginalRowAndMailerUntouched(t *testing.T) {
	for _, reason := range []string{"expired", "cipher missing", "cipher corrupt", "key changed", "recipient changed", "host changed", "purpose binding changed", "gate lost"} {
		t.Run(reason, func(t *testing.T) {
			r, w, m, p := dueFixture(t)
			m.answer = "pending"
			if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
				t.Fatal(err)
			}
			id := asString(m.calls[0]["requestId"])
			switch reason {
			case "expired":
				p.now = func() time.Time { return dueNow.Add(14 * 24 * time.Hour) }
			case "cipher missing":
				delete(r.requests[id], "tokenCiphertext")
			case "cipher corrupt":
				r.requests[id]["tokenCiphertext"] = "not a sealed envelope"
			case "key changed":
				t.Setenv(secret.EnvMasterKey, strings.Repeat("29", 32))
			case "recipient changed":
				r.orders[0]["email"] = "another@example.com"
			case "host changed":
				site := map[string]any{}
				for k, v := range r.sites[0] {
					site[k] = v
				}
				site["hostname"] = "new.example.test"
				r.sites[0] = site
			case "purpose binding changed":
				r.requests[id]["issuedAt"] = dueNow.Add(time.Minute)
			case "gate lost":
				p.coordinateRequest = func(context.Context, string) (requestLease, error) {
					return &fakeRequestLease{err: errors.New("lost")}, nil
				}
			}
			writes, mails := len(w.calls), len(m.calls)
			if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
				t.Fatal(err)
			}
			if len(w.calls) != writes || len(m.calls) != mails {
				t.Fatal("unsafe retry mutated a request or attempted mail")
			}
		})
	}
}

func TestMissingMasterKeyDoesNotWriteOrSend(t *testing.T) {
	_, w, m, p := dueFixture(t)
	t.Setenv(secret.EnvMasterKey, "")
	if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 0 || len(m.calls) != 0 {
		t.Fatal("missing key permitted a request")
	}
}

func TestRetryCiphertextIsRegisteredAsSecret(t *testing.T) {
	memqldsl.RegisterTree(Domain, Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(Domain) })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	concept, err := memorynodes.DefaultRegistry().Get(requestConcept)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range concept.SecretFields() {
		if field == "tokenCiphertext" {
			return
		}
	}
	t.Fatal("retry ciphertext is not marked secret for generic organization exports")
}

func TestRecordedErasureStopsPendingRetryBeforeCleanup(t *testing.T) {
	r, w, m, p := dueFixture(t)
	m.answer = "pending"
	if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
		t.Fatal(err)
	}
	id := asString(m.calls[0]["requestId"])
	p.reader = retryEraseReader{rowReader: r, reviewID: reviewIDForRequest(id), requestID: id, store: "store-live", owner: "merchant-1"}
	writes, mails := len(w.calls), len(m.calls)
	if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != writes || len(m.calls) != mails {
		t.Fatal("a terminal erase decision allowed a retry before data cleanup")
	}
}

func TestErasedRequestTokenIsClosedWithoutAReviewRead(t *testing.T) {
	request := openRequest()
	request["erasedAt"] = linkNow
	p, _ := linkProvider(&linkReader{request: request})
	_, err := p.openLink(linkCaller(), "store-live", theToken, "floss-picks")
	if code := refusalCode(t, err); code != RefusalLinkUsed {
		t.Fatalf("erased token refusal %s", code)
	}
}

type retryMailerFunc func(context.Context, map[string]any) (string, string, error)

func (f retryMailerFunc) SendOrderMessage(ctx context.Context, args map[string]any) (string, string, error) {
	return f(ctx, args)
}

func TestProviderErrorCannotPersistPlaintextBearer(t *testing.T) {
	_, w, _, p := dueFixture(t)
	var token string
	p.mailer = retryMailerFunc(func(_ context.Context, args map[string]any) (string, string, error) {
		token = asString(args["fields"].(map[string]any)["reviewToken"])
		return "", "", errors.New("provider echoed message token: " + token)
	})
	if _, err := p.sendDueRequests(fromAutomation(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("test did not reach mail route")
	}
	for _, write := range w.calls {
		raw, _ := json.Marshal(write.args)
		if strings.Contains(string(raw), token) {
			t.Fatal("request write persisted a bearer from a provider error")
		}
	}
}

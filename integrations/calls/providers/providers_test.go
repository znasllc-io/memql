package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// The adapter must form the real HTTPS host/path. Only the test transport
		// substitutes a loopback fixture, so tests make no external provider calls.
		if r.URL.Scheme != "https" || (r.URL.Host != "api.stripe.com" && r.URL.Host != "verification.didit.me") {
			t.Errorf("unexpected destination %s", r.URL)
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = target.Scheme
		u.Host = target.Host
		clone.URL = &u
		return http.DefaultTransport.RoundTrip(clone)
	})}
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func sign(data []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
func TestModesCannotDispatchLiveOrMockRequests(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("mode gate allowed network I/O"); return nil, nil })}
	for _, mode := range []Mode{Disabled, Mock, ""} {
		stripe, err := NewStripe(StripeConfig{Mode: mode, Client: client})
		if err != nil {
			t.Fatal(err)
		}
		_, err = stripe.CreateCheckout(context.Background(), CheckoutRequest{})
		if !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrMockOperation) {
			t.Fatal(err)
		}
		didit, err := NewDidit(DiditConfig{Mode: mode, Client: client})
		if err != nil {
			t.Fatal(err)
		}
		_, err = didit.CreateVerification(context.Background(), VerificationRequest{})
		if !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrMockOperation) {
			t.Fatal(err)
		}
	}
	for _, config := range []StripeConfig{{Mode: "live", SecretKey: "sk_live_no"}, {Mode: Sandbox, SecretKey: "sk_live_no"}, {Mode: Sandbox, SecretKey: ""}} {
		if _, err := NewStripe(config); err == nil {
			t.Fatal("accepted live/unconfigured key")
		}
	}
	if _, err := NewDidit(DiditConfig{Mode: "live", APIKey: "key"}); err == nil {
		t.Fatal("accepted live Didit")
	}
}
func TestStripeCheckoutBindingAndSavedMethodOptIn(t *testing.T) {
	for _, save := range []bool{false, true} {
		t.Run(fmt.Sprint(save), func(t *testing.T) {
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/checkout/sessions" {
					t.Fatalf("unexpected %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer sk_test_fixture" || r.Header.Get("Idempotency-Key") != "operation_1" || r.Header.Get("Stripe-Version") != StripeAPIVersion {
					t.Fatal("missing protocol headers")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				for k, v := range map[string]string{"client_reference_id": "account_1", "metadata[operation_id]": "operation_1", "line_items[0][price_data][unit_amount]": "1000", "line_items[0][price_data][currency]": "usd", "payment_method_types[0]": "card", "payment_intent_data[metadata][account_id]": "account_1"} {
					if r.Form.Get(k) != v {
						t.Errorf("%s=%s", k, r.Form.Get(k))
					}
				}
				if (r.Form.Get("payment_intent_data[setup_future_usage]") == "off_session") != save {
					t.Fatal("saved payment method consent ignored")
				}
				if (r.Form.Get("customer_creation") == "always") != save {
					t.Fatal("customer creation consent ignored")
				}
				respond(w, map[string]any{"id": "cs_test_1", "url": "https://checkout.stripe.com/test", "status": "open", "payment_status": "unpaid", "amount_total": 1000, "currency": "usd", "livemode": false, "client_reference_id": "account_1", "metadata": map[string]string{"operation_id": "operation_1"}})
			})
			stripe, err := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			got, err := stripe.CreateCheckout(context.Background(), CheckoutRequest{OperationID: "operation_1", AccountID: "account_1", AmountCents: 1000, ReturnURL: "http://localhost:5173/paid", CancelURL: "http://localhost:5173/cancel", SaveMethod: save})
			if err != nil || got.ID != "cs_test_1" || got.Paid || got.OperationID != "operation_1" || got.AccountID != "account_1" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}
func TestStripeAuthoritativeReadAndPartialRefund(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("expand[]") != "payment_intent.latest_charge" {
			t.Fatal("missing authoritative expansion")
		}
		respond(w, map[string]any{"id": "cs_test_1", "status": "complete", "payment_status": "paid", "amount_total": 1000, "currency": "usd", "livemode": false, "client_reference_id": "account_1", "metadata": map[string]string{"operation_id": "operation_1"}, "payment_intent": map[string]any{"id": "pi_1", "status": "succeeded", "amount": 1000, "amount_received": 1000, "currency": "usd", "customer": "cus_1", "payment_method": "pm_1", "latest_charge": map[string]any{"refunded": false, "amount_refunded": 100}}})
	})
	stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
	got, err := stripe.ReadCheckout(context.Background(), "cs_test_1")
	if err != nil || !got.Paid || !got.Refunded || got.PaymentMethodID != "pm_1" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestStripeOffSessionUsesFixedMandateAndIdempotency(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payment_intents" || r.Method != "POST" || r.Header.Get("Idempotency-Key") != "topup_1" {
			t.Fatal("bad payment operation")
		}
		_ = r.ParseForm()
		for k, v := range map[string]string{"off_session": "true", "confirm": "true", "amount": "1000", "customer": "cus_1", "payment_method": "pm_1"} {
			if r.Form.Get(k) != v {
				t.Fatal(k)
			}
		}
		respond(w, map[string]any{"id": "pi_1", "status": "requires_action", "amount": 1000, "amount_received": 0, "currency": "usd", "customer": "cus_1", "payment_method": "pm_1", "metadata": map[string]string{"operation_id": "topup_1", "account_id": "account_1"}})
	})
	stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
	got, err := stripe.ChargeOffSession(context.Background(), ChargeRequest{OperationID: "topup_1", AccountID: "account_1", AmountCents: 1000, CustomerID: "cus_1", PaymentMethodID: "pm_1"})
	if err != nil || got.Paid || !got.RequiresAction || got.OperationID != "topup_1" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestUncertainWritesNeverRetryOrReleaseAsRejected(t *testing.T) {
	for _, status := range []int{500, 408} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var count atomic.Int32
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"ambiguous"}`)
			})
			stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
			_, err := stripe.ChargeOffSession(context.Background(), ChargeRequest{OperationID: "topup_1", AccountID: "account_1", AmountCents: 1000, CustomerID: "cus_1", PaymentMethodID: "pm_1"})
			if !errors.Is(err, ErrOutcomeUnknown) || count.Load() != 1 {
				t.Fatalf("retry/ambiguity lost: %v calls=%d", err, count.Load())
			}
		})
	}
}
func TestProviderRedirectDoesNotFollowOrLeakCredential(t *testing.T) {
	var count atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		http.Redirect(w, r, "https://attacker.invalid/collect", http.StatusTemporaryRedirect)
	})
	stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
	if _, err := stripe.ReadCheckout(context.Background(), "cs_test_1"); err == nil {
		t.Fatal("accepted provider redirect")
	}
	if count.Load() != 1 {
		t.Fatal("followed provider redirect")
	}
}
func approvedDidit() map[string]any {
	return map[string]any{"session_id": "session_1", "status": "Approved", "vendor_data": "account_1", "workflow_id": "workflow_1", "workflow_version": 3, "environment": "sandbox", "metadata": map[string]string{"operation_id": "verify_1"}, "id_verifications": []any{map[string]any{"status": "Approved", "age": 32, "verification_method": "document", "date_of_birth": "1994-01-01", "full_name": "Must not escape adapter"}}, "liveness_checks": []any{map[string]any{"status": "Approved"}}, "face_matches": []any{map[string]any{"status": "Approved"}}}
}
func TestDiditAlwaysForcesSandboxAndKeepsAccountBinding(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v3/session/" || r.Header.Get("X-Api-Key") != "fixture" {
			t.Fatal("bad Didit protocol")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["sandbox_scenario"] != "approve" || body["vendor_data"] != "account_1" || body["workflow_id"] != "workflow_1" || body["callback"] != "http://localhost:5173/verified" {
			t.Fatalf("bad sandbox body %v", body)
		}
		v := approvedDidit()
		v["status"] = "Not Started"
		v["url"] = "https://verify.didit.me/session/test"
		respond(w, v)
	})
	didit, _ := NewDidit(DiditConfig{Mode: Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 3, Client: client})
	got, err := didit.CreateVerification(context.Background(), VerificationRequest{OperationID: "verify_1", AccountID: "account_1", CallbackURL: "http://localhost:5173/verified"})
	if err != nil || got.Adult18 || got.AccountID != "account_1" || got.OperationID != "verify_1" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestDiditRequiresDocumentAgeLivenessAndFaceEvidence(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		adult   bool
		failure bool
	}{
		{"adult", func(map[string]any) {}, true, false},
		{"identity status alone", func(v map[string]any) { delete(v, "id_verifications") }, false, false},
		{"missing age", func(v map[string]any) { delete(v["id_verifications"].([]any)[0].(map[string]any), "age") }, false, false},
		{"underage", func(v map[string]any) { v["id_verifications"].([]any)[0].(map[string]any)["age"] = 17 }, false, false},
		{"liveness absent", func(v map[string]any) { delete(v, "liveness_checks") }, false, false},
		{"face review", func(v map[string]any) { v["face_matches"].([]any)[0].(map[string]any)["status"] = "In Review" }, false, false},
		{"manual override", func(v map[string]any) { v["status_override"] = map[string]any{"status": "Approved"} }, false, false},
		{"wrong workflow", func(v map[string]any) { v["workflow_id"] = "another" }, false, true},
		{"unreviewed version", func(v map[string]any) { v["workflow_version"] = 4 }, false, true},
		{"live response", func(v map[string]any) { v["environment"] = "live" }, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { v := approvedDidit(); tc.mutate(v); respond(w, v) })
			didit, _ := NewDidit(DiditConfig{Mode: Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 3, Client: client})
			got, err := didit.ReadVerification(context.Background(), "session_1")
			if (err != nil) != tc.failure || got.Adult18 != tc.adult {
				t.Fatalf("%+v %v", got, err)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "1994") || strings.Contains(string(data), "Must not escape") {
				t.Fatal("retained identity fields")
			}
		})
	}
}
func TestStripeWebhookIntegrityReplayEnvironmentAndExtraHeaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	secret := "whsec_fixture"
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","livemode":false,"created":1800000000,"data":{"object":{"id":"cs_test_1","client_reference_id":"account_1","metadata":{"operation_id":"op_1"}}}}`)
	header := "t=1800000000,v0=ignore,v1=wrong,other=ignored,v1=" + sign(append([]byte("1800000000."), body...), secret)
	got, err := VerifyStripeWebhook(body, header, secret, now)
	if err != nil || got.ID != "evt_1" || got.ObjectID != "cs_test_1" || got.AccountID != "account_1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = VerifyStripeWebhook(body, header, secret, now.Add(301*time.Second)); err == nil {
		t.Fatal("accepted stale event")
	}
	if _, err = VerifyStripeWebhook(append(body, ' '), header, secret, now); err == nil {
		t.Fatal("accepted changed bytes")
	}
	live := []byte(strings.Replace(string(body), `"livemode":false`, `"livemode":true`, 1))
	liveHeader := "t=1800000000,v1=" + sign(append([]byte("1800000000."), live...), secret)
	if _, err = VerifyStripeWebhook(live, liveHeader, secret, now); !errors.Is(err, ErrWrongEnvironment) {
		t.Fatal("accepted live event")
	}
}
func TestDiditV2UnicodeCanonicalRawFallbackAndNoSimpleDowngrade(t *testing.T) {
	now := time.Unix(1800000000, 0)
	secret := "didit_secret"
	body := []byte(`{"webhook_type":"status.updated","timestamp":1800000000,"session_id":"session_1","status":"Approved","vendor_data":"account_1","environment":"sandbox","event_id":"evt_1","created_at":1800000000,"metadata":{"name":"José <名字>"},"decision":{"score":100.0}}`)
	canonical := []byte(`{"created_at":1800000000,"decision":{"score":100},"environment":"sandbox","event_id":"evt_1","metadata":{"name":"José <名字>"},"session_id":"session_1","status":"Approved","timestamp":1800000000,"vendor_data":"account_1","webhook_type":"status.updated"}`)
	headers := http.Header{}
	headers.Set("X-Timestamp", "1800000000")
	headers.Set("X-Signature-V2", sign(canonical, secret))
	got, err := VerifyDiditWebhook(body, headers, secret, now)
	if err != nil || got.ID != "evt_1" {
		t.Fatalf("%+v %v", got, err)
	}
	tampered := []byte(strings.Replace(string(body), `"score":100.0`, `"score":0`, 1))
	if _, err = VerifyDiditWebhook(tampered, headers, secret, now); err == nil {
		t.Fatal("accepted tampered decision")
	}
	headers.Del("X-Signature-V2")
	headers.Set("X-Signature-Simple", sign([]byte("1800000000:session_1:Approved:status.updated"), secret))
	if _, err = VerifyDiditWebhook(body, headers, secret, now); err == nil {
		t.Fatal("accepted deprecated envelope-only signature")
	}
	headers.Set("X-Signature-Previous", sign(body, secret))
	if _, err = VerifyDiditWebhook(body, headers, secret, now); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyDiditWebhook(body, headers, secret, now.Add(-301*time.Second)); err == nil {
		t.Fatal("accepted future timestamp")
	}
}

func TestStripeHTTP402PreservesExistingIntentForOwnerAuthentication(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		respond(w, map[string]any{"error": map[string]any{"code": "authentication_required", "payment_intent": map[string]any{"id": "pi_challenge", "status": "requires_action", "amount": 1000, "amount_received": 0, "currency": "usd", "customer": "cus_1", "payment_method": "pm_1", "metadata": map[string]string{"account_id": "account_1", "operation_id": "topup_1"}}}})
	})
	stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
	got, err := stripe.ChargeOffSession(context.Background(), ChargeRequest{OperationID: "topup_1", AccountID: "account_1", AmountCents: 1000, CustomerID: "cus_1", PaymentMethodID: "pm_1"})
	if err != nil || got.ID != "pi_challenge" || !got.RequiresAction || got.Paid {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestDiditReadRequestsWorkflowProvenance(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("include") != "events" {
			t.Fatal("Didit plain decision omits workflow_version")
		}
		respond(w, approvedDidit())
	})
	didit, _ := NewDidit(DiditConfig{Mode: Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 3, Client: client})
	if got, err := didit.ReadVerification(context.Background(), "session_1"); err != nil || !got.Adult18 {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestInvalidArgumentsNeverReachProviders(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid arguments reached provider")
		return nil, nil
	})}
	stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
	for _, r := range []CheckoutRequest{
		{OperationID: "operation_1", AccountID: "account_1", AmountCents: 999, ReturnURL: "https://example.com/paid", CancelURL: "https://example.com/cancel"},
		{OperationID: "operation_1", AccountID: "account_1", AmountCents: 1000, ReturnURL: "javascript:alert(1)", CancelURL: "https://example.com/cancel"},
		{OperationID: "../bad", AccountID: "account_1", AmountCents: 1000, ReturnURL: "https://example.com/paid", CancelURL: "https://example.com/cancel"},
	} {
		if _, err := stripe.CreateCheckout(context.Background(), r); err == nil {
			t.Fatal("accepted invalid checkout")
		}
	}
	if _, err := stripe.ReadCheckout(context.Background(), "cs_live_1"); err == nil {
		t.Fatal("accepted live checkout identifier")
	}
	didit, _ := NewDidit(DiditConfig{Mode: Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 3, Client: client})
	if _, err := didit.ReadVerification(context.Background(), "../another"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestStripeRefundAndDisputeWebhooksLocatePaymentIntent(t *testing.T) {
	now := time.Unix(1800000000, 0)
	secret := "whsec_fixture"
	for _, eventType := range []string{"charge.refunded", "charge.dispute.created", "charge.dispute.closed"} {
		t.Run(eventType, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"id": "evt_refund", "type": eventType, "livemode": false, "created": now.Unix(), "data": map[string]any{"object": map[string]any{"id": "ch_or_du_1", "payment_intent": "pi_paid", "metadata": map[string]string{"account_id": "account_1", "operation_id": "op_1"}}}})
			header := "t=1800000000,v1=" + sign(append([]byte("1800000000."), body...), secret)
			got, err := VerifyStripeWebhook(body, header, secret, now)
			if err != nil || got.ObjectID != "pi_paid" || got.OperationID != "op_1" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}
func TestStripeAuthoritativeDisputeFlag(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"id": "pi_paid", "status": "succeeded", "amount": 1000, "amount_received": 1000, "currency": "usd", "latest_charge": map[string]any{"refunded": false, "amount_refunded": 0, "disputed": true}})
	})
	stripe, _ := NewStripe(StripeConfig{Mode: Sandbox, SecretKey: "sk_test_fixture", Client: client})
	got, err := stripe.ReadPayment(context.Background(), "pi_paid")
	if err != nil || !got.Disputed || got.Refunded {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestDiditExposesOnlyUnderageConclusionForFinalDocumentFailure(t *testing.T) {
	for _, byWarning := range []bool{false, true} {
		t.Run(fmt.Sprint(byWarning), func(t *testing.T) {
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				v := approvedDidit()
				v["status"] = "Declined"
				doc := v["id_verifications"].([]any)[0].(map[string]any)
				doc["status"] = "Declined"
				if byWarning {
					delete(doc, "age")
					doc["warnings"] = []any{map[string]any{"code": "MINIMUM_AGE_NOT_MET"}}
				} else {
					doc["age"] = 17
				}
				respond(w, v)
			})
			didit, _ := NewDidit(DiditConfig{Mode: Sandbox, APIKey: "fixture", WorkflowID: "workflow_1", WorkflowVersion: 3, Client: client})
			got, err := didit.ReadVerification(context.Background(), "session_1")
			if err != nil || !got.Underage || got.Adult18 {
				t.Fatalf("%+v %v", got, err)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "date_of_birth") || strings.Contains(string(data), "1994") {
				t.Fatal("identity data escaped")
			}
		})
	}
}

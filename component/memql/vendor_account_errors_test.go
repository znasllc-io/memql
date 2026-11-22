package memql

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
)

// The bodies below are the shapes the vendors document (read 2026-11-22):
// Anthropic's error envelope and the spend-cap 429 from its rate-limits page,
// OpenAI's error object and the codes on its error-codes page.

const anthropicSpendCapBody = `{"type":"error","error":{"type":"rate_limit_error","message":"You have reached your API usage limits: your organization has crossed its monthly API usage threshold, set based on your organization's API tier. You will regain access on 2026-09-01 at 00:00 UTC.","details":{"error_code":"enforced_spend_limit_reached"}},"request_id":"req_018EeWyXxfu5pfWkrYcMdjWG"}`

func anthropicBody(errorType, message string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q},"request_id":"req_011CSHoEeqs5C35K2UUqR7Fy"}`, errorType, message)
}

func openaiBody(errorType, code, message string) string {
	codeJSON := "null"
	if code != "" {
		codeJSON = fmt.Sprintf("%q", code)
	}
	return fmt.Sprintf(`{"error":{"message":%q,"type":%q,"param":null,"code":%s}}`, message, errorType, codeJSON)
}

func TestClassifyVendorAnswer(t *testing.T) {
	cases := []struct {
		name   string
		vendor string
		status int
		body   string
		want   string
	}{
		{"anthropic revoked key", VendorAnthropic, 401, anthropicBody("authentication_error", "invalid x-api-key"), accountFailureRefused},
		{"anthropic no permission", VendorAnthropic, 403, anthropicBody("permission_error", "Your API key does not have permission to use the specified resource."), accountFailurePermission},
		{"anthropic billing", VendorAnthropic, 402, anthropicBody("billing_error", "There is an issue with your billing or payment information."), accountFailureCredit},
		{"anthropic organization spend limit", VendorAnthropic, 400, anthropicBody("invalid_request_error", "You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC."), accountFailureCredit},
		{"anthropic workspace spend limit", VendorAnthropic, 400, anthropicBody("invalid_request_error", "You have reached your specified workspace API usage limits. You will regain access on 2026-12-01 at 00:00 UTC."), accountFailureCredit},
		{"anthropic tier spend cap", VendorAnthropic, 429, anthropicSpendCapBody, accountFailureCredit},
		{"anthropic rate limit", VendorAnthropic, 429, anthropicBody("rate_limit_error", "This request would exceed your organization's rate limit of 50 requests per minute."), ""},
		{"anthropic ordinary bad request", VendorAnthropic, 400, anthropicBody("invalid_request_error", "messages: roles must alternate between \"user\" and \"assistant\""), ""},
		{"anthropic model not found", VendorAnthropic, 404, anthropicBody("not_found_error", "model: claude-nope"), ""},
		{"anthropic overloaded", VendorAnthropic, 529, anthropicBody("overloaded_error", "Overloaded"), ""},
		{"anthropic server error", VendorAnthropic, 500, anthropicBody("api_error", "Internal server error"), ""},

		{"openai incorrect key", VendorOpenAI, 401, openaiBody("invalid_request_error", "invalid_api_key", "Incorrect API key provided: sk-proj-abcdEFGH1234****************wxyz. You can find your API key at https://platform.openai.com/account/api-keys."), accountFailureRefused},
		{"openai missing scope", VendorOpenAI, 401, openaiBody("invalid_request_error", "", "You have insufficient permissions for this operation. Missing scopes: model.request. Check that you have the correct role in your organization."), accountFailurePermission},
		{"openai missing scope as 403", VendorOpenAI, 403, openaiBody("invalid_request_error", "", "You have insufficient permissions for this operation. Missing scopes: api.model.read."), accountFailurePermission},
		{"openai region", VendorOpenAI, 403, openaiBody("request_forbidden", "unsupported_country_region_territory", "Country, region, or territory not supported"), accountFailurePermission},
		{"openai credit exhausted", VendorOpenAI, 429, openaiBody("insufficient_quota", "credit_balance_exhausted", "Your organization has no prepaid credits remaining."), accountFailureCredit},
		{"openai organization spend limit", VendorOpenAI, 429, openaiBody("invalid_request_error", "organization_spend_limit_exceeded", "Organization spend limit reached."), accountFailureCredit},
		{"openai project spend limit", VendorOpenAI, 429, openaiBody("invalid_request_error", "project_spend_limit_exceeded", "Project spend limit reached."), accountFailureCredit},
		{"openai organization usage limit", VendorOpenAI, 429, openaiBody("invalid_request_error", "organization_usage_limit_exceeded", "Organization usage limit reached."), accountFailureCredit},
		{"openai quota by type", VendorOpenAI, 429, openaiBody("insufficient_quota", "", "You exceeded your current quota."), accountFailureCredit},
		{"openai quota by code", VendorOpenAI, 429, openaiBody("invalid_request_error", "insufficient_quota", "You exceeded your current quota."), accountFailureCredit},
		{"openai rate limit", VendorOpenAI, 429, openaiBody("rate_limit_error", "slow_down", "Slow down."), ""},
		{"openai overloaded", VendorOpenAI, 503, openaiBody("service_unavailable_error", "server_is_overloaded", "The model is overloaded."), ""},
		{"openai server error", VendorOpenAI, 500, openaiBody("server_error", "", "The server had an error while processing your request."), ""},

		{"a success is no failure", VendorAnthropic, 200, `{}`, ""},
		{"a vendor nobody knows", "someone-else", 401, `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyVendorAnswer(tc.vendor, tc.status, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// OpenAI's SDK hands back the inner error object rather than the envelope, so
// the same answer has to classify from either shape.
func TestClassifyVendorAnswerReadsABareErrorObject(t *testing.T) {
	bare := `{"message":"Your organization has no prepaid credits remaining.","type":"insufficient_quota","param":null,"code":"credit_balance_exhausted"}`
	if got, _ := classifyVendorAnswer(VendorOpenAI, 429, []byte(bare)); got != accountFailureCredit {
		t.Fatalf("kind = %q, want %q", got, accountFailureCredit)
	}
}

func TestClassifyReasonIsTheVendorsWordsScrubbed(t *testing.T) {
	body := openaiBody("invalid_request_error", "invalid_api_key",
		"Incorrect API key provided: sk-proj-abcdEFGH1234****************wxyz. You can find your API key at https://platform.openai.com/account/api-keys.")
	_, reason := classifyVendorAnswer(VendorOpenAI, 401, []byte(body))
	if !strings.Contains(reason, "Incorrect API key provided") {
		t.Errorf("the reason lost the vendor's words: %q", reason)
	}
	if strings.Contains(reason, "sk-proj-") || strings.Contains(reason, "wxyz") {
		t.Errorf("the reason still carries part of a key: %q", reason)
	}

	// A body with no message still says something a person can act on.
	_, reason = classifyVendorAnswer(VendorAnthropic, 401, []byte(`not json`))
	if reason != "Anthropic answered 401" {
		t.Errorf("reason for a body with no message = %q, want %q", reason, "Anthropic answered 401")
	}

	// And a long one is cut, so a ledger row and a state row stay small.
	long := anthropicBody("authentication_error", strings.Repeat("x", 2000))
	if _, reason = classifyVendorAnswer(VendorAnthropic, 401, []byte(long)); len(reason) > 300 {
		t.Errorf("reason is %d characters, want at most 300", len(reason))
	}
}

func TestScrubKeyShapes(t *testing.T) {
	for _, secret := range []string{
		"sk-ant-api03-AbC_123-xyzXYZ",
		"sk-proj-****abcd",
		"sk-admin-1234abcd",
		"sk-abcdEFGH1234****************wxyz",
		"sk-svcacct-AbCdEf123456",
	} {
		got := scrubKeyShapes("the key " + secret + " was refused")
		if strings.Contains(got, secret) {
			t.Errorf("%q survived: %q", secret, got)
		}
		if got != "the key sk-[removed] was refused" {
			t.Errorf("scrubbed text = %q", got)
		}
	}
	for _, harmless := range []string{"task-list", "sk-", "risk-free pricing", "ask-me"} {
		if got := scrubKeyShapes(harmless); got != harmless {
			t.Errorf("%q was changed to %q", harmless, got)
		}
	}
}

// answering is a stand-in vendor that gives one status and body to everything.
func answering(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func anthropicErrorFrom(t *testing.T, status int, body string) error {
	t.Helper()
	srv := answering(t, status, body)
	client := anthropic.NewClient(
		anthropicoption.WithBaseURL(srv.URL),
		anthropicoption.WithAPIKey("stand-in"),
		anthropicoption.WithMaxRetries(0),
	)
	_, err := client.Models.List(context.Background(), anthropic.ModelListParams{})
	if err == nil {
		t.Fatalf("the stand-in answered %d and the SDK returned no error", status)
	}
	return err
}

func openaiErrorFrom(t *testing.T, status int, body string) error {
	t.Helper()
	srv := answering(t, status, body)
	client := openai.NewClient(
		openaioption.WithBaseURL(srv.URL+"/v1"),
		openaioption.WithAPIKey("stand-in"),
		openaioption.WithMaxRetries(0),
	)
	_, err := client.Models.List(context.Background())
	if err == nil {
		t.Fatalf("the stand-in answered %d and the SDK returned no error", status)
	}
	return err
}

// The router sees whatever a provider returned, wrapped or not. This is the
// question it asks of it.
func TestVendorAccountFailureReadsSDKErrors(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("anthropic stream pre-flight: %w", err) }

	cases := []struct {
		name     string
		err      error
		wantKind string
		wantOK   bool
	}{
		{"anthropic 401", wrap(anthropicErrorFrom(t, 401, anthropicBody("authentication_error", "invalid x-api-key"))), accountFailureRefused, true},
		{"anthropic 402", anthropicErrorFrom(t, 402, anthropicBody("billing_error", "billing")), accountFailureCredit, true},
		{"anthropic 403", anthropicErrorFrom(t, 403, anthropicBody("permission_error", "no")), accountFailurePermission, true},
		{"anthropic spend cap", anthropicErrorFrom(t, 429, anthropicSpendCapBody), accountFailureCredit, true},
		{"anthropic rate limit", anthropicErrorFrom(t, 429, anthropicBody("rate_limit_error", "slow")), "", false},
		{"anthropic 500", anthropicErrorFrom(t, 500, anthropicBody("api_error", "oops")), "", false},
		{"openai 401", wrap(openaiErrorFrom(t, 401, openaiBody("invalid_request_error", "invalid_api_key", "Incorrect API key provided: sk-proj-abcdEFGH1234****************wxyz."))), accountFailureRefused, true},
		{"openai credit", openaiErrorFrom(t, 429, openaiBody("insufficient_quota", "credit_balance_exhausted", "no credits")), accountFailureCredit, true},
		{"openai missing scope", openaiErrorFrom(t, 401, openaiBody("invalid_request_error", "", "You have insufficient permissions for this operation. Missing scopes: model.request.")), accountFailurePermission, true},
		{"openai rate limit", openaiErrorFrom(t, 429, openaiBody("rate_limit_error", "slow_down", "Slow down.")), "", false},
		{"openai 500", openaiErrorFrom(t, 500, openaiBody("server_error", "", "oops")), "", false},
		{"the engine's own refusal", wrap(&VendorAccountError{Vendor: VendorOpenAI, Kind: accountFailureUnavailable, Reason: "the cluster has no account at OpenAI"}), accountFailureUnavailable, true},
		{"a sign-in that is waiting", wrap(&vendorSignInWaiting{Vendor: VendorAnthropic}), accountFailureUnavailable, true},
		{"a sign-in the vendor refused", wrap(&vendorExchangeRefused{Status: 401, Body: "no"}), accountFailureUnavailable, true},
		{"a cancelled caller", context.Canceled, "", false},
		{"any other error", errors.New("connection reset"), "", false},
		{"no error", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := VendorAccountFailure(tc.err)
			if kind != tc.wantKind || ok != tc.wantOK {
				t.Fatalf("VendorAccountFailure = (%q, %v), want (%q, %v)", kind, ok, tc.wantKind, tc.wantOK)
			}
		})
	}
}

func TestVendorAccountErrorSaysWhatAndWhere(t *testing.T) {
	err := &VendorAccountError{Vendor: VendorAnthropic, Kind: accountFailureUnavailable, Reason: "the cluster has no account at Anthropic"}
	if got := err.Error(); got != "vendor account unavailable: the cluster has no account at Anthropic" {
		t.Fatalf("Error() = %q", got)
	}
}

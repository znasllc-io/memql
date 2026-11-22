package memql

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// vendor_key_probe_test.go -- checking an API key without spending a token
// (design record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K3, K11 and K12, safety rule S6).

// A stand-in key for each vendor. Neither is a key: each only has the prefix
// the shape check asks for.
const (
	probeAnthropicKey = "sk-ant-stand-in-for-a-test"
	probeOpenAIKey    = "sk-stand-in-for-a-test"
)

func probeKeyFor(vendor string) string {
	if vendor == VendorAnthropic {
		return probeAnthropicKey
	}
	return probeOpenAIKey
}

// The check lists the vendor's models and sends no prompt: one GET, no POST,
// carrying the key the way that vendor takes it and nothing of the cluster's.
func TestProbeListsModelsAndSendsNoPrompt(t *testing.T) {
	for _, vendor := range []string{VendorAnthropic, VendorOpenAI} {
		t.Run(vendor, func(t *testing.T) {
			stand := newAccountVendor(t, vendor)
			reg := newProviderRegistry()
			before := time.Now().UTC().Add(-time.Second)

			got := reg.ProbeVendorKey(context.Background(), vendor, probeKeyFor(vendor))
			if got.Verdict != VendorKeyProbeGood || got.Models != 2 || got.Detail != "" {
				t.Fatalf("probe = %+v, want good with the two models the stand-in lists", got)
			}
			if got.At.Before(before) || got.At.After(time.Now().UTC().Add(time.Second)) {
				t.Errorf("the evidence is stamped %v, want when the check started", got.At)
			}
			seen := stand.seen()
			if len(seen) != 1 {
				t.Fatalf("the check made %d requests, want one", len(seen))
			}
			request := seen[0]
			if request.Method != http.MethodGet || !strings.HasSuffix(request.Path, "/models") {
				t.Fatalf("the check sent %s %s, want one GET of the model list", request.Method, request.Path)
			}
			switch vendor {
			case VendorAnthropic:
				if request.Header.Get("X-Api-Key") != probeAnthropicKey || request.Header.Get("Authorization") != "" {
					t.Errorf("Anthropic was sent X-Api-Key=%q Authorization=%q", request.Header.Get("X-Api-Key"), request.Header.Get("Authorization"))
				}
			case VendorOpenAI:
				if request.Header.Get("Authorization") != "Bearer "+probeOpenAIKey {
					t.Errorf("OpenAI was sent Authorization=%q", request.Header.Get("Authorization"))
				}
				for _, cluster := range []string{"OpenAI-Project", "OpenAI-Organization"} {
					if request.Header.Get(cluster) != "" {
						t.Errorf("the check carried the cluster's %s beside a person's key", cluster)
					}
				}
			}
		})
	}
}

func TestProbeVerdicts(t *testing.T) {
	echoed := "sk-proj-abcdEFGH1234****************wxyz"
	cases := []struct {
		name    string
		vendor  string
		answer  accountVendorAnswer
		verdict string
		detail  string
	}{
		{"Anthropic refuses the key", VendorAnthropic,
			accountVendorAnswer{Status: 401, Body: anthropicBody("authentication_error", "invalid x-api-key")},
			VendorKeyProbeRefused, "invalid x-api-key"},
		{"Anthropic's account has a billing problem", VendorAnthropic,
			accountVendorAnswer{Status: 402, Body: anthropicBody("billing_error", "Your credit balance is too low")},
			VendorKeyProbeCredit, "Your credit balance is too low"},
		{"Anthropic's monthly spend cap", VendorAnthropic,
			accountVendorAnswer{Status: 429, Body: anthropicSpendCapBody},
			VendorKeyProbeCredit, ""},
		{"an Anthropic key that may not list models is not saved", VendorAnthropic,
			accountVendorAnswer{Status: 403, Body: anthropicBody("permission_error", "Your API key does not have permission to use the specified resource.")},
			VendorKeyProbeRefused, "does not have permission"},
		{"Anthropic is rate limiting", VendorAnthropic,
			accountVendorAnswer{Status: 429, Body: anthropicBody("rate_limit_error", "slow down")},
			VendorKeyProbeUnreachable, "Anthropic"},
		{"Anthropic is down", VendorAnthropic,
			accountVendorAnswer{Status: 529, Body: anthropicBody("overloaded_error", "Overloaded")},
			VendorKeyProbeUnreachable, "Anthropic"},
		{"OpenAI refuses the key, and echoes part of it", VendorOpenAI,
			accountVendorAnswer{Status: 401, Body: openaiBody("invalid_request_error", "invalid_api_key", "Incorrect API key provided: "+echoed+".")},
			VendorKeyProbeRefused, "Incorrect API key provided"},
		{"an OpenAI key restricted from listing models is checked by use", VendorOpenAI,
			accountVendorAnswer{Status: 401, Body: openaiBody("invalid_request_error", "", "You have insufficient permissions for this operation. Missing scopes: api.model.read.")},
			VendorKeyProbeUseOnly, "Missing scopes"},
		{"OpenAI's account is out of quota", VendorOpenAI,
			accountVendorAnswer{Status: 429, Body: openaiBody("insufficient_quota", "insufficient_quota", "You exceeded your current quota")},
			VendorKeyProbeCredit, "exceeded your current quota"},
		{"OpenAI is down", VendorOpenAI,
			accountVendorAnswer{Status: 503, Body: openaiBody("server_error", "", "The server is overloaded")},
			VendorKeyProbeUnreachable, "OpenAI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stand := newAccountVendor(t, tc.vendor)
			stand.answerNext(tc.answer)
			got := newProviderRegistry().ProbeVendorKey(context.Background(), tc.vendor, probeKeyFor(tc.vendor))
			if got.Verdict != tc.verdict {
				t.Fatalf("verdict = %q (%q), want %q", got.Verdict, got.Detail, tc.verdict)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to say %q", got.Detail, tc.detail)
			}
			for _, secret := range []string{probeKeyFor(tc.vendor), echoed, "sk-proj-"} {
				if strings.Contains(got.Detail, secret) {
					t.Errorf("the detail carries something shaped like a key: %q", got.Detail)
				}
			}
			if len(stand.seen()) != 1 {
				t.Errorf("the check made %d requests, want one and no retry", len(stand.seen()))
			}
		})
	}

	t.Run("the vendor cannot be reached", func(t *testing.T) {
		stand := newAccountVendor(t, VendorAnthropic)
		stand.srv.Close()
		got := newProviderRegistry().ProbeVendorKey(context.Background(), VendorAnthropic, probeAnthropicKey)
		if got.Verdict != VendorKeyProbeUnreachable {
			t.Fatalf("verdict = %q, want unreachable: no answer is not a verdict on the key", got.Verdict)
		}
		if strings.Contains(got.Detail, probeAnthropicKey) || strings.Contains(got.Detail, stand.srv.URL) {
			t.Errorf("the detail repeats the key or an address: %q", got.Detail)
		}
	})
}

// K3: a key is sent to the vendor's own address and nowhere else. On a cluster
// that sends the vendor's calls somewhere else, a key could never be used, so
// it is refused before any request is made.
func TestProbeRefusesAnotherAddress(t *testing.T) {
	stand := newAccountVendor(t, VendorOpenAI)
	reg := newProviderRegistry()
	// The cluster's record dials the stand-in; a key may only go to OpenAI.
	record, err := newOpenAIProvider(stand.config("chat", false))
	if err != nil {
		t.Fatal(err)
	}
	reg.setEntry(&ProviderConfigEntry{Config: stand.config("chat", false), Client: record})
	vendorKeyOrigins[VendorOpenAI] = "https://api.openai.com"

	if reason := reg.VendorKeysRefusedHere(VendorOpenAI); !strings.Contains(reason, "another address") {
		t.Fatalf("VendorKeysRefusedHere = %q, want the cluster's address named as the reason", reason)
	}
	got := reg.ProbeVendorKey(context.Background(), VendorOpenAI, probeOpenAIKey)
	if got.Verdict != VendorKeyProbeUnreachable || !strings.Contains(got.Detail, "another address") {
		t.Fatalf("probe = %+v, want it refused for the cluster's address", got)
	}
	if len(stand.seen()) != 0 {
		t.Fatalf("the key was sent to an address that is not the vendor's own: %+v", stand.seen())
	}
	// The other vendor is unaffected.
	if reason := reg.VendorKeysRefusedHere(VendorAnthropic); reason != "" {
		t.Errorf("Anthropic keys are refused because of an OpenAI record: %q", reason)
	}
}

// S6: a value that cannot be a key for the chosen vendor is refused before
// anything is asked of the vendor, in words that never repeat it.
func TestKeyShapeRefusals(t *testing.T) {
	long := "sk-ant-" + strings.Repeat("a", 600)
	cases := []struct {
		name, vendor, key string
		refused           bool
		says              string
	}{
		{"an Anthropic key", VendorAnthropic, probeAnthropicKey, false, ""},
		{"an OpenAI key", VendorOpenAI, probeOpenAIKey, false, ""},
		{"an OpenAI project key", VendorOpenAI, "sk-proj-stand-in", false, ""},
		{"nothing", VendorAnthropic, "", true, "Paste"},
		{"only spaces", VendorOpenAI, "   ", true, "Paste"},
		{"a space inside", VendorAnthropic, "sk-ant-stand in", true, "spaces"},
		{"a line break inside", VendorOpenAI, "sk-stand\nin", true, "spaces"},
		{"too long", VendorAnthropic, long, true, "too long"},
		{"an Anthropic Admin key", VendorAnthropic, "sk-ant-admin01-stand-in", true, "Admin"},
		{"an OpenAI admin key", VendorOpenAI, "sk-admin-stand-in", true, "admin"},
		{"an OpenAI key given as Anthropic", VendorAnthropic, probeOpenAIKey, true, "sk-ant-"},
		{"an Anthropic key given as OpenAI", VendorOpenAI, probeAnthropicKey, true, "Anthropic"},
		{"not a key at all", VendorOpenAI, "hunter2-password", true, "sk-"},
		{"another vendor", "mistral", "sk-stand-in", true, "Anthropic or OpenAI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VendorKeyShapeRefusal(tc.vendor, tc.key)
			if (got != "") != tc.refused {
				t.Fatalf("refusal = %q, want refused=%v", got, tc.refused)
			}
			if !strings.Contains(got, tc.says) {
				t.Errorf("refusal = %q, want it to say %q", got, tc.says)
			}
			if trimmed := strings.TrimSpace(tc.key); len(trimmed) > 8 && strings.Contains(got, trimmed) {
				t.Errorf("the refusal repeats the value: %q", got)
			}
			if tc.key == long && strings.Contains(got, strings.Repeat("a", 20)) {
				t.Errorf("the refusal repeats part of the value")
			}
		})
	}
}

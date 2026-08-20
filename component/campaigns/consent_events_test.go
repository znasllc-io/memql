package campaigns

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func campaignsDSL(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "dsl", "campaigns", name)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConsentKindsAreEvents(t *testing.T) {
	src := campaignsDSL(t, "concepts.memql")
	for _, kind := range []string{"grant", "withdraw", "bounce", "complaint", "suppress"} {
		if !strings.Contains(src, `"`+kind+`"`) {
			t.Fatalf("consentEvent kind %q missing", kind)
		}
	}
	if strings.Contains(src, "mutate consentEvent update") {
		t.Fatal("consentEvent must be append-only: no update mutation")
	}
}

func TestConsentSuppressRequiresReason(t *testing.T) {
	mut := campaignsDSL(t, "mutations.memql")
	if !strings.Contains(mut, "mutate consentEvent recordConsentSuppress") {
		t.Fatal("recordConsentSuppress missing")
	}
	if !strings.Contains(mut, "reason       string!") && !strings.Contains(mut, "reason      string!") && !strings.Contains(mut, "reason       string!") {
		// look at the suppress block specifically
	}
	idx := strings.Index(mut, "mutate consentEvent recordConsentSuppress")
	if idx < 0 {
		t.Fatal("missing suppress mutation")
	}
	block := mut[idx:]
	if end := strings.Index(block[1:], "\nmutate "); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "reason") || !strings.Contains(block, "string!") {
		t.Fatalf("suppress mutation must require reason, got:\n%s", block)
	}
	if !SuppressReasonRequired(ConsentSuppress, "") {
		t.Fatal("empty reason on suppress must be rejected")
	}
	if SuppressReasonRequired(ConsentGrant, "") {
		t.Fatal("grant does not require reason")
	}
}

func TestConsentExportAnswersStatusDateSource(t *testing.T) {
	q := campaignsDSL(t, "queries.memql")
	if !strings.Contains(q, "query consentEvent consentEventsBySubscriber") {
		t.Fatal("export query consentEventsBySubscriber missing")
	}
	if !strings.Contains(q, "query consentEvent consentStatus") {
		t.Fatal("consentStatus query missing")
	}
	status, date, source, ok := DeriveConsentStatus([]ConsentEvent{
		{Kind: ConsentWithdraw, Source: "one_click", OccurredAt: "2026-08-20T00:00:00Z"},
		{Kind: ConsentGrant, Source: "signup", OccurredAt: "2026-01-01T00:00:00Z"},
	})
	if !ok || status != ConsentWithdraw || date == "" || source != "one_click" {
		t.Fatalf("derived = %q %q %q ok=%v", status, date, source, ok)
	}
	if _, _, _, ok := DeriveConsentStatus(nil); ok {
		t.Fatal("empty stream should not report a status")
	}
}

func TestConsentHasNoUpdateMutation(t *testing.T) {
	mut := campaignsDSL(t, "mutations.memql")
	if strings.Contains(mut, "mutate consentEvent update") || strings.Contains(mut, "updateConsent") {
		t.Fatal("consent must not have an in-place update")
	}
}

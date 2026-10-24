package apps

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func planToken(t *testing.T, claim map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}
func TestCodexPlanClaim(t *testing.T) {
	for _, tc := range []struct{ plan, want string }{{"pro", SubscriptionPresent}, {"plus", SubscriptionPresent}, {"free", SubscriptionNone}, {"", ""}, {"unknown", SubscriptionUnknown}} {
		t.Run(tc.plan, func(t *testing.T) {
			token := planToken(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": tc.plan}})
			if got := codexReportedPlan(token); got != tc.want {
				t.Fatalf("plan = %q, want %q", got, tc.want)
			}
		})
	}
	for _, token := range []string{"", "broken", "x.bad!.y", planToken(t, map[string]any{"exp": 1, "https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro"}}), planToken(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()})} {
		if got := codexReportedPlan(token); got != "" {
			t.Fatalf("invalid/expired claim produced %q", got)
		}
	}
}
func TestCodexAuthReadsPlanWithoutChangingSignIn(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	token := planToken(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro"}})
	raw, _ := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": token}})
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	got := probeCodex(home)
	if !got.signedIn || got.subscription != SubscriptionPresent {
		t.Fatalf("state = %+v", got)
	}
}

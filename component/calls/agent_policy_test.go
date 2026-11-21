package calls

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentPolicyPurposeAndManualAdmissionContract(t *testing.T) {
	for _, s := range []string{"", " too short ", strings.Repeat("x", 501), "hidden\u0085line", "line\ncontrol"} {
		if ValidAgentPurpose(s) {
			t.Fatalf("unsafe purpose accepted %q", s)
		}
	}
	if !ValidAgentPurpose(strings.Repeat("界", 500)) {
		t.Fatal("Unicode purpose bounded by runes, not bytes")
	}
	manual := UTCPManual("https://calls.example/_memql/calls/agent")
	if manual["manual_version"] != "2.4.0" {
		t.Fatal("admission contract version not bumped")
	}
	data, e := json.Marshal(manual["agent_policy"])
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(data), AgentConductDigest) {
		t.Fatal("manual omitted immutable policy digest")
	}
	seen := map[string]bool{}
	for _, raw := range manual["tools"].([]any) {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		seen[name] = true
		if name != "callkey_start" && name != "callkey_join" && name != "callkey_schedule" && name != "callkey_reschedule" && name != "callkey_topup" && name != "callkey_guest_join" {
			continue
		}
		req := tool["inputs"].(map[string]any)["properties"].(map[string]any)["request"].(map[string]any)
		fields := req["properties"].(map[string]any)
		if _, ok := fields["acceptance"]; !ok {
			t.Fatalf("%s omits acceptance", name)
		}
		required := req["required"].([]string)
		joined := strings.Join(required, ",")
		if !strings.Contains(joined, "acceptance") || !strings.Contains(joined, "idempotencyKey") {
			t.Fatalf("%s acceptance/retry binding optional", name)
		}
	}
	for _, name := range []string{"callkey_processing_consent", "callkey_authority", "callkey_request_join", "callkey_join_status", "callkey_guest_join"} {
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
}

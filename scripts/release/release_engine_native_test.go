package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func nativeReleaseFixture() map[string]any {
	return map[string]any{
		"id": 7, "name": "MemQL / widget / release", "app": map[string]any{"id": 123},
		"head_sha": "headsha", "external_id": "native-run", "details_url": "https://os.example.test/pipelines/run/native-run",
		"status": "in_progress", "conclusion": nil, "started_at": "2026-01-01T00:00:05Z",
	}
}

func nativeChecksJSON(t *testing.T, pages ...[]map[string]any) string {
	t.Helper()
	var out []map[string]any
	for _, checks := range pages {
		out = append(out, map[string]any{"check_runs": checks})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReleaseEngineRequiresItsAppReleaseEventAndExactCommit(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{"another app", "app", map[string]any{"id": 999}},
		{"PR context", "name", "MemQL / widget"},
		{"push context", "name", "MemQL / widget / push"},
		{"another commit", "head_sha", "other-sha"},
		{"older run", "started_at", "2025-12-31T23:59:59Z"},
		{"not started", "started_at", nil},
		{"missing run identity", "external_id", ""},
		{"missing run link", "details_url", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := nativeReleaseFixture()
			check[tc.key] = tc.value
			r := runReleaseEngine(t, map[string]string{"FAKE_RELEASE_STATE": "published", "FAKE_NATIVE_CHECKS": nativeChecksJSON(t, []map[string]any{check})},
				"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
			if r.code != 5 {
				t.Fatalf("unrelated or incomplete evidence accepted: exit=%d", r.code)
			}
		})
	}
}

func TestReleaseEngineRefusesMissingOrUnboundNativePolicyBeforePublishing(t *testing.T) {
	for _, policy := range []string{
		`[]`,
		`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"MemQL / widget"}]}}]`,
		`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"MemQL / widget","integration_id":123},{"context":"MemQL / other","integration_id":123}]}}]`,
	} {
		r := runReleaseEngine(t, map[string]string{"FAKE_VERSION": "1.2.3\n", "FAKE_NATIVE_RULES": policy},
			"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
		if r.code != 5 || len(r.calls("release create")) != 0 {
			t.Fatalf("unverified policy permitted a release: exit=%d", r.code)
		}
	}
}

func TestReleaseEngineReportsNativeRunAndRejectsTerminalFailure(t *testing.T) {
	for _, conclusion := range []string{"success", "failure", "cancelled", "timed_out", "neutral", ""} {
		t.Run(conclusion, func(t *testing.T) {
			check := nativeReleaseFixture()
			check["status"], check["conclusion"] = "completed", conclusion
			r := runReleaseEngine(t, map[string]string{"FAKE_RELEASE_STATE": "published", "FAKE_NATIVE_CHECKS": nativeChecksJSON(t, []map[string]any{check})},
				"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
			want := 5
			if conclusion == "success" {
				want = 0
			}
			if r.code != want {
				t.Fatalf("exit=%d, want %d", r.code, want)
			}
			result := r.envelope(t)["result"].(map[string]any)
			if result["buildRunId"] != "native-run" || result["buildRunConclusion"] != conclusion {
				t.Fatalf("lost native evidence: %v", result)
			}
			if len(r.calls("run list")) != 0 {
				t.Fatal("still consulted Actions")
			}
		})
	}
}

func TestReleaseEngineReadsAllCheckPagesAndChoosesNewestMatchingRun(t *testing.T) {
	old := nativeReleaseFixture()
	old["status"], old["conclusion"] = "completed", "failure"
	newer := nativeReleaseFixture()
	newer["id"], newer["external_id"] = 8, "new-native-run"
	r := runReleaseEngine(t, map[string]string{"FAKE_RELEASE_STATE": "published", "FAKE_NATIVE_CHECKS": nativeChecksJSON(t, []map[string]any{old}, []map[string]any{newer})},
		"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
	if r.code != 0 {
		t.Fatalf("new native run was not found: exit=%d", r.code)
	}
	result := r.envelope(t)["result"].(map[string]any)
	if result["buildRunId"] != "new-native-run" {
		t.Fatalf("wrong run: %v", result)
	}
	calls := r.calls("check-runs?")
	if len(calls) != 1 || !strings.Contains(calls[0], "--paginate --slurp") {
		t.Fatalf("not all check pages read: %v", calls)
	}
}

func TestReleaseEngineRejectsInvalidPollBudgetBeforeAnyRepositoryCall(t *testing.T) {
	for _, value := range []string{"-1", "no", "1+2", "21601", "999999999999999999999"} {
		r := runReleaseEngine(t, nil, "--version=1.2.3", "--pollSeconds="+value)
		if r.code != 2 || len(r.calls("api ")) > 0 || len(r.calls("release ")) > 0 {
			t.Fatalf("invalid poll budget %q reached repository or was accepted", value)
		}
	}
}

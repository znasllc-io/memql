package deploycontrol

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// --- semver helper --------------------------------------------------------

func TestParseSemver(t *testing.T) {
	ok := []struct {
		in              string
		maj, min, patch int
	}{
		{"0.0.0", 0, 0, 0},
		{"1.2.3", 1, 2, 3},
		{"0.9.9", 0, 9, 9},
		{" 2026.6.21 ", 2026, 6, 21}, // CalVer + surrounding whitespace
		{"10.20.30", 10, 20, 30},
	}
	for _, tc := range ok {
		v, err := parseSemver(tc.in)
		if err != nil {
			t.Errorf("parseSemver(%q) err = %v", tc.in, err)
			continue
		}
		if v.major != tc.maj || v.minor != tc.min || v.patch != tc.patch {
			t.Errorf("parseSemver(%q) = %+v, want %d.%d.%d", tc.in, v, tc.maj, tc.min, tc.patch)
		}
	}

	bad := []string{"", "1", "1.2", "1.2.3.4", "1.2.x", "v1.2.3", "1.2.-3", "1.2.3-rc1", "1.2.3+build", "a.b.c", "1..3"}
	for _, in := range bad {
		if _, err := parseSemver(in); err == nil {
			t.Errorf("parseSemver(%q) expected error, got nil", in)
		}
	}
}

func TestSemverBump(t *testing.T) {
	v, _ := parseSemver("1.2.3")
	cases := map[string]string{
		"major": "2.0.0",
		"minor": "1.3.0",
		"patch": "1.2.4",
		"":      "1.2.4", // empty defaults to patch
	}
	for part, want := range cases {
		got, err := v.bump(part)
		if err != nil {
			t.Errorf("bump(%q) err = %v", part, err)
			continue
		}
		if got.String() != want {
			t.Errorf("bump(%q) = %q, want %q", part, got.String(), want)
		}
	}
	if _, err := v.bump("bogus"); err == nil {
		t.Error("bump(bogus) expected error")
	}

	// CalVer bump stays CalVer-shaped.
	cal, _ := parseSemver("2026.6.21")
	if got, _ := cal.bump("patch"); got.String() != "2026.6.22" {
		t.Errorf("CalVer patch bump = %q, want 2026.6.22", got.String())
	}
	if got, _ := cal.bump("minor"); got.String() != "2026.7.0" {
		t.Errorf("CalVer minor bump = %q, want 2026.7.0", got.String())
	}
}

func TestSemverCompare(t *testing.T) {
	a, _ := parseSemver("1.2.3")
	b, _ := parseSemver("1.2.4")
	c, _ := parseSemver("1.3.0")
	d, _ := parseSemver("2.0.0")
	if a.compare(b) >= 0 || b.compare(c) >= 0 || c.compare(d) >= 0 {
		t.Error("compare ordering wrong for ascending versions")
	}
	if a.compare(a) != 0 {
		t.Error("compare(self) != 0")
	}
	if d.compare(a) <= 0 {
		t.Error("2.0.0 should be greater than 1.2.3")
	}
}

// fakeEngine + newTestServiceWithEngine live in deployment_store_test.go.

// deploymentNode builds a v1:cluster:deployment query-result node with
// the fields currentVersionForEnv reads.
func deploymentNode(env, status, version string) *memqlv1.MemoryNode {
	st, _ := structpb.NewStruct(map[string]any{
		"environment": env,
		"status":      status,
		"version":     version,
	})
	return &memqlv1.MemoryNode{Payload: st}
}

// mutationsOf returns the mutation* queries the engine was asked to run.
func mutationsOf(eng *fakeEngine) []string {
	var out []string
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "mutation") {
			out = append(out, q)
		}
	}
	return out
}

// --- SuggestNextVersion ---------------------------------------------------

func TestSuggestNextVersionFromLatestSucceeded(t *testing.T) {
	// Highest succeeded prod version is 1.4.2; the 1.5.0 row is failed so
	// it must NOT be the base.
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		deploymentNode("production", "succeeded", "1.4.0"),
		deploymentNode("production", "succeeded", "1.4.2"),
		deploymentNode("production", "failed", "1.5.0"),
		deploymentNode("staging", "succeeded", "9.9.9"), // other env -- ignored
	}}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)

	res, err := svc.SuggestNextVersion(ctxWithRole(auth.RoleOwner), &memqlv1.SuggestNextVersionRequest{Env: "prod"})
	if err != nil {
		t.Fatalf("SuggestNextVersion: %v", err)
	}
	if res.GetCurrentVersion() != "1.4.2" {
		t.Errorf("current = %q, want 1.4.2", res.GetCurrentVersion())
	}
	if res.GetSource() != "deployment" {
		t.Errorf("source = %q, want deployment", res.GetSource())
	}
	if res.GetNextMajor() != "2.0.0" || res.GetNextMinor() != "1.5.0" || res.GetNextPatch() != "1.4.3" {
		t.Errorf("proposals = major %q / minor %q / patch %q", res.GetNextMajor(), res.GetNextMinor(), res.GetNextPatch())
	}
}

func TestSuggestNextVersionNoHistorySeedsFromZero(t *testing.T) {
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
	res, err := svc.SuggestNextVersion(ctxWithRole(auth.RoleAdmin), &memqlv1.SuggestNextVersionRequest{Env: "staging"})
	if err != nil {
		t.Fatalf("SuggestNextVersion: %v", err)
	}
	if res.GetCurrentVersion() != "" || res.GetSource() != "none" {
		t.Errorf("current = %q source = %q, want empty/none", res.GetCurrentVersion(), res.GetSource())
	}
	if res.GetNextMajor() != "1.0.0" || res.GetNextMinor() != "0.1.0" || res.GetNextPatch() != "0.0.1" {
		t.Errorf("seed proposals = major %q / minor %q / patch %q", res.GetNextMajor(), res.GetNextMinor(), res.GetNextPatch())
	}
}

func TestSuggestNextVersionDeniesNonAdmin(t *testing.T) {
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, &fakeEngine{})
	_, err := svc.SuggestNextVersion(ctxWithRole(auth.RoleWriter), &memqlv1.SuggestNextVersionRequest{Env: "prod"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

func TestSuggestNextVersionInvalidEnv(t *testing.T) {
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
	_, err := svc.SuggestNextVersion(ctxWithRole(auth.RoleOwner), &memqlv1.SuggestNextVersionRequest{Env: "dev"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// --- CutVersion -----------------------------------------------------------

func TestCutVersionDefaultsToPatchAndCreatesPending(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		deploymentNode("production", "succeeded", "1.4.2"),
	}}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, eng)

	res, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod"})
	if err != nil {
		t.Fatalf("CutVersion: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	if res.GetDetails()["version"] != "1.4.3" {
		t.Errorf("cut version = %q, want 1.4.3 (patch default)", res.GetDetails()["version"])
	}
	if res.GetDetails()["bump"] != "patch" {
		t.Errorf("bump label = %q, want patch", res.GetDetails()["bump"])
	}
	if res.GetDetails()["deploymentId"] == "" {
		t.Error("deploymentId empty")
	}
	// Exactly one mutationCreateDeployment with status pending + version.
	if len(mutationsOf(eng)) != 1 {
		t.Fatalf("mutation calls = %d, want 1", len(mutationsOf(eng)))
	}
	call := mutationsOf(eng)[0]
	if !strings.HasPrefix(call, "mutationCreateDeployment(") {
		t.Errorf("unexpected mutation: %s", call)
	}
	if !strings.Contains(call, `status: "pending"`) {
		t.Errorf("create should set status pending: %s", call)
	}
	if !strings.Contains(call, `version: "1.4.3"`) {
		t.Errorf("create should carry version 1.4.3: %s", call)
	}
	if !strings.Contains(call, `environment: "production"`) {
		t.Errorf("create should target production: %s", call)
	}
	// One success audit event.
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeSuccess {
		t.Fatalf("want one success audit event, got %+v", audit.events)
	}
	if audit.events[0].Action != "deployment_console_cut_version" {
		t.Errorf("audit action = %q", audit.events[0].Action)
	}
}

func TestCutVersionMajorMinor(t *testing.T) {
	for _, tc := range []struct{ bump, want string }{
		{"major", "2.0.0"},
		{"minor", "1.5.0"},
	} {
		eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{deploymentNode("production", "succeeded", "1.4.2")}}
		svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)
		res, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod", Bump: tc.bump})
		if err != nil {
			t.Fatalf("bump %s: %v", tc.bump, err)
		}
		if res.GetDetails()["version"] != tc.want {
			t.Errorf("bump %s -> %q, want %q", tc.bump, res.GetDetails()["version"], tc.want)
		}
	}
}

func TestCutVersionExplicitOverride(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{deploymentNode("production", "succeeded", "1.4.2")}}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)
	res, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod", Version: "3.0.0", Bump: "patch"})
	if err != nil {
		t.Fatalf("CutVersion: %v", err)
	}
	if res.GetDetails()["version"] != "3.0.0" {
		t.Errorf("explicit version = %q, want 3.0.0 (bump ignored)", res.GetDetails()["version"])
	}
	if res.GetDetails()["bump"] != "explicit" {
		t.Errorf("bump label = %q, want explicit", res.GetDetails()["bump"])
	}
}

func TestCutVersionRejectsInvalidVersion(t *testing.T) {
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
	_, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod", Version: "not-a-version"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestCutVersionRejectsInvalidBump(t *testing.T) {
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
	_, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod", Bump: "huge"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestCutVersionRejectsDuplicate(t *testing.T) {
	// 1.4.3 already exists (pending) for prod -> patch-bumping 1.4.2 lands
	// on 1.4.3, which must be rejected as a duplicate (no new write).
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		deploymentNode("production", "succeeded", "1.4.2"),
		deploymentNode("production", "pending", "1.4.3"),
	}}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, eng)

	res, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod"})
	if err != nil {
		t.Fatalf("CutVersion (duplicate) returned status err = %v, want ActionResult ok=false", err)
	}
	if res.GetOk() {
		t.Error("ok = true, want false on duplicate")
	}
	if !strings.Contains(res.GetMessage(), "already exists") {
		t.Errorf("message = %q, want duplicate notice", res.GetMessage())
	}
	if len(mutationsOf(eng)) != 0 {
		t.Errorf("duplicate must NOT create a record, got %v", mutationsOf(eng))
	}
	// Rejection is still an authorized action -> one failure audit event.
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Fatalf("want one failure audit event, got %+v", audit.events)
	}
}

func TestCutVersionDeniesNonAdminWithBlockedAudit(t *testing.T) {
	eng := &fakeEngine{}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, eng)
	_, err := svc.CutVersion(ctxWithRole(auth.RoleReader), &memqlv1.CutVersionRequest{Env: "prod"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
	if len(mutationsOf(eng)) != 0 {
		t.Error("denied caller must not create a record")
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

func TestCutVersionSurfacesWriteFailure(t *testing.T) {
	eng := &fakeEngine{
		queryNodes:  []*memqlv1.MemoryNode{deploymentNode("production", "succeeded", "1.4.2")},
		mutationErr: errors.New("db down"),
	}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, eng)
	res, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Env: "prod"})
	if err != nil {
		t.Fatalf("CutVersion returned status err = %v, want ActionResult ok=false", err)
	}
	if res.GetOk() {
		t.Error("ok = true, want false on write failure")
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Fatalf("want one failure audit event, got %+v", audit.events)
	}
}

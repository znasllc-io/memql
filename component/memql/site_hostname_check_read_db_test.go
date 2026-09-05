package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// site_hostname_check_read_db_test.go -- the two address checks (2026-09-05
// design, D7), through the real engine so the answer is the write guard's
// own probe and not a re-derivation of it.

// hostnameCheck drives the evaluator the executor registry dispatches to
// (executor_builtin.go), the way TestProviderVerifyIsOwnerOnly drives its
// sibling. The DSL declaration's executor NAME is checked by
// initBuiltinExecutorHandlers at the shared engine's Init, which this test
// depends on -- a declared executor with no handler refuses that Init.
func hostnameCheck(t *testing.T, ctx context.Context, eng *MemQLEngine, builtin, hostname string) map[string]any {
	t.Helper()
	args := map[string]any{"hostname": hostname}
	var nodes []memorynodes.MemoryNode
	var err error
	switch builtin {
	case "siteHostnameCheck":
		nodes, err = eng.evaluateSiteHostnameCheckExpression(ctx, args)
	case "customDomainCheck":
		nodes, err = eng.evaluateCustomDomainCheckExpression(ctx, args)
	default:
		t.Fatalf("no such check %q", builtin)
	}
	if err != nil {
		t.Fatalf("%s(%q): %v", builtin, hostname, err)
	}
	if len(nodes) != 1 {
		t.Fatalf("%s(%q): want one row, got %d", builtin, hostname, len(nodes))
	}
	var out map[string]any
	if jerr := json.Unmarshal(nodes[0].Payload, &out); jerr != nil {
		t.Fatalf("%s(%q): payload is not JSON: %v", builtin, hostname, jerr)
	}
	return out
}

func TestSiteHostnameCheckAnswersWhatTheWriteGuardWouldSay(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	suffix := uniqueSuffix("check")
	caller := userSiteCtx("user-check-" + suffix)

	taken := "taken-" + suffix + "." + siteTestDomain
	id := "site-check-" + suffix
	if _, err := createSiteRaw(t, caller, eng, map[string]any{
		"siteId": id, "hostname": taken, "bundleRef": "blob://sites/" + id + "/v1/",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Taken, and NOBODY is named: the refusal on a write names the holder,
	// a check asked on every keystroke does not.
	row := hostnameCheck(t, caller, eng, "siteHostnameCheck", taken)
	if row["available"] != false || row["reason"] != "taken" {
		t.Fatalf("a held hostname must read taken: %v", row)
	}
	if strings.Contains(stringFromAny(row["problem"]), id) {
		t.Fatalf("the check must not name the holder: %v", row)
	}

	// Free.
	row = hostnameCheck(t, caller, eng, "siteHostnameCheck", "free-"+suffix+"."+siteTestDomain)
	if row["available"] != true || row["reason"] != "ok" || row["problem"] != "" {
		t.Fatalf("a free hostname must read available with no problem: %v", row)
	}

	// The shape half, in the policy's own words: reserved, and off-domain.
	row = hostnameCheck(t, caller, eng, "siteHostnameCheck", "identity."+siteTestDomain)
	if row["available"] != false || row["reason"] != "invalid" || !strings.Contains(stringFromAny(row["problem"]), "reserved") {
		t.Fatalf("a reserved label must read invalid with the policy's sentence: %v", row)
	}
	// UNIQUE, because the database is shared across sessions and a sibling
	// test creates a site at a fixed off-domain hostname under a privileged
	// caller -- which would make this read "taken" rather than "invalid".
	elsewhere := "shop-" + suffix + ".somewhere-else.test"
	row = hostnameCheck(t, caller, eng, "siteHostnameCheck", elsewhere)
	if row["available"] != false || row["reason"] != "invalid" {
		t.Fatalf("another domain must read invalid for a user: %v", row)
	}

	// ...and WAIVED for a cluster owner, exactly as the guard waives it: a
	// custom apex is an operator's legitimate deployment.
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner-" + suffix, Role: auth.RoleOwner})
	owner = auth.ContextWithToken(owner, &auth.TokenInfo{Subject: "owner-" + suffix})
	row = hostnameCheck(t, owner, eng, "siteHostnameCheck", elsewhere)
	if row["available"] != true {
		t.Fatalf("a cluster owner may claim a hostname off the domain, and the check must say so: %v", row)
	}
}

func TestCustomDomainCheckIsAClusterOwnersQuestion(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	suffix := uniqueSuffix("dcheck")

	user := userSiteCtx("user-dcheck-" + suffix)
	if _, err := eng.evaluateCustomDomainCheckExpression(user, map[string]any{"hostname": "www.acme.test"}); err == nil {
		t.Fatal("a non-owner asked about a custom domain and was answered")
	}

	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner-" + suffix, Role: auth.RoleOwner})
	owner = auth.ContextWithToken(owner, &auth.TokenInfo{Subject: "owner-" + suffix})

	row := hostnameCheck(t, owner, eng, "customDomainCheck", "www.acme-"+suffix+".test")
	if row["available"] != true {
		t.Fatalf("a free client domain must read available: %v", row)
	}
	// Under the cluster's own domain is not a custom domain -- the policy's
	// sentence says to create the site with that name instead.
	row = hostnameCheck(t, owner, eng, "customDomainCheck", "shop."+siteTestDomain)
	if row["available"] != false || row["reason"] != "invalid" || !strings.Contains(stringFromAny(row["problem"]), "own domain") {
		t.Fatalf("a hostname under the cluster's domain must read invalid: %v", row)
	}
	// A hostname a SITE already serves is taken for a domain too.
	taken := "dtaken-" + suffix + "." + siteTestDomain
	id := "site-dcheck-" + suffix
	if _, err := createSiteRaw(t, userSiteCtx("user-dcheck2-"+suffix), eng, map[string]any{
		"siteId": id, "hostname": taken, "bundleRef": "blob://sites/" + id + "/v1/",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// (that one is under the cluster's domain, so it reads invalid before
	// taken -- the shape rule comes first, as it does on the write.)
	row = hostnameCheck(t, owner, eng, "customDomainCheck", taken)
	if row["available"] != false {
		t.Fatalf("a served hostname must not read available: %v", row)
	}
}

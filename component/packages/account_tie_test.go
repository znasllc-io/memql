package packages

import (
	"context"
	"strings"
	"testing"
)

// The account tie rides from the package onto its run (memql#5303, design
// D12).
//
// v1:platform:packageDeployment declares the same `account="accountId"`
// argument its package does, and the ONLY writer of a deployment row's
// account is openDeployment, copying it off the package row the starting
// caller already resolved -- the same borrowed-authority shape ownerUserId
// takes one line above it. A run of a tied package is therefore readable by
// exactly the people the package is; a run of an untied package carries no
// account and reads as it always did.

func TestOpeningARunCopiesThePackagesAccountOntoIt(t *testing.T) {
	pkg := ownerPackage()
	pkg["accountId"] = "self"
	h := newHarness(t, spaOnlyPackage(), pkg)

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	open := statementContaining(h.engine.queries, "mutation openPackageDeployment")
	if open == "" {
		t.Fatal("no run was opened")
	}
	if !strings.Contains(open, `accountId: "self"`) {
		t.Errorf("the run does not carry its package's account, so the people the package "+
			"is tied to cannot read its timeline:\n%s", open)
	}
}

// AN UNTIED PACKAGE OPENS AN UNTIED RUN. Every package written before the
// field existed has no account, and its runs must go on reading exactly as
// they did: owner and cluster owner, nobody else. The seed renders the
// argument only when it has a value, so the statement for an untied package
// is byte-for-byte what it was.
func TestOpeningARunOfAnUntiedPackageRecordsNoAccount(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	open := statementContaining(h.engine.queries, "mutation openPackageDeployment")
	if open == "" {
		t.Fatal("no run was opened")
	}
	if strings.Contains(open, "accountId") {
		t.Errorf("an untied package's run names an account:\n%s", open)
	}
}

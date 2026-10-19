package azureblob

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

func systemForwardedContext(t *testing.T, subject string) context.Context {
	t.Helper()
	authority, err := auth.ForwardedAuthorityForSystem(subject, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return auth.BindForwardedContext(context.Background(), authority.Principal().Claims, access, authority)
}

// A build forwarded to the workbench runs under the engine's own SYSTEM-class
// authority, named for a node ("workbench-build:<node>"), never a person. What
// it writes under packages/ belongs to the cluster, so the quota must not look
// that name up as a user.
func TestStorageOwnerOfASystemForwardedBuildIsTheCluster(t *testing.T) {
	for _, subject := range []string{"workbench-build:workbench-5fdd9c4d6b-26gcp", "pipelines-executor:planner-1"} {
		ctx := systemForwardedContext(t, subject)
		owner, err := storageOwner(ctx, "packages/builds/2dabee4b/storefront/output.tar.gz")
		if err != nil || owner != "" {
			t.Fatalf("%s: owner=%q err=%v; want the cluster (empty), not a user lookup", subject, owner, err)
		}
	}
}

// The exemption is the verified system assertion and nothing else: a person is
// still charged, even one whose context carries a system assertion naming a
// different subject.
func TestOnlyAnEnginesOwnWritesAreClusterOwned(t *testing.T) {
	person := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "person", Role: "user"})
	if got := quotaOwner(person); got != "person" {
		t.Fatalf("a person's write was owned by %q", got)
	}

	authority, err := auth.ForwardedAuthorityForSystem("workbench-build:node-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mismatched := auth.ContextWithForwardedAuthority(person, authority)
	if got := quotaOwner(mismatched); got != "person" {
		t.Fatalf("a person carrying someone else's system assertion was owned by %q", got)
	}

	if got := quotaOwner(systemForwardedContext(t, "workbench-build:node-1")); got != "" {
		t.Fatalf("the engine's own write was owned by %q", got)
	}
}

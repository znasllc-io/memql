package azureblob

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestStorageOwnerMatchesUserIdentityAcrossRoles(t *testing.T) {
	for _, role := range []auth.Role{"owner", "developer", "admin", "writer", "user", "reader", "viewer"} {
		for _, caller := range []string{"person", "v1:identity:user:person"} {
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: caller, Role: role})
			for _, prefix := range []string{"library", "backups", "objects"} {
				for _, pathOwner := range []string{"person", "v1:identity:user:person"} {
					owner, err := storageOwner(ctx, prefix+"/"+pathOwner+"/file/document.md")
					if err != nil || owner != "person" {
						t.Fatalf("%s caller %s path %s: owner=%q err=%v", role, caller, pathOwner, owner, err)
					}
				}
				for _, other := range []string{"another", "v1:identity:user:another", "v1:agents:agent:person"} {
					if _, err := storageOwner(ctx, prefix+"/"+other+"/file/document.md"); err == nil {
						t.Fatalf("%s caller %s admitted another identity %s", role, caller, other)
					}
				}
			}
		}
	}
}

func TestStorageOwnerChargesSystemWritesToThePathOwner(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "operator", Synthetic: true})
	owner, err := storageOwner(ctx, "library/v1:identity:user:person/file/document.md")
	if err != nil || owner != "person" {
		t.Fatalf("system write lost its owner: %q %v", owner, err)
	}
	owner, err = storageOwner(ctx, "logs/day/node.gz")
	if err != nil || owner != "" {
		t.Fatalf("cluster asset acquired a personal owner: %q %v", owner, err)
	}
}

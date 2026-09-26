package memql

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestSourceConnectionWriteRequiresVerifiedWriter(t *testing.T) {
	e := &MemQLEngine{}
	ctx := rankActorCtx("operator", auth.RoleOwner)
	for _, prior := range []map[string]any{nil, {"ownerUserId": "operator", "status": "active"}} {
		if err := e.validateSourceConnectionWrite(ctx, "v1:platform:sourceConnection", prior, map[string]any{"installationId": "forged"}); err == nil {
			t.Fatal("raw client write bypassed provider verification")
		}
		if err := e.validateSourceConnectionWrite(auth.ContextWithInternalOrigin(ctx), "v1:platform:sourceConnection", prior, map[string]any{"installationId": "verified"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackageConnectionValidationDoesNotTrustClientBindings(t *testing.T) {
	e := &MemQLEngine{}
	ctx := rankActorCtx("owner", auth.RoleOwner)
	payload := map[string]any{"sourceConnectionId": "source-a", "sourceKind": "repo", "credentialId": "grant-a", "repoUrl": "https://github.com/a/repo"}
	if err := e.validateSourceConnectionWrite(ctx, "v1:platform:package", nil, payload); err == nil {
		t.Fatal("missing validator admitted a provider binding")
	}
	calls := 0
	refused := errors.New("repository is outside this source")
	e.builtinExecutorHandlers = map[string]builtinExecutorHandler{}
	e.builtinExecutorHandlers["integration.packages.validateSourceConnectionRepository"] = func(gotCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		calls++
		ac, _ := auth.AccessFromContext(gotCtx)
		if ac == nil || ac.UserId != "owner" {
			t.Fatal("validation lost original caller")
		}
		if args["connectionId"] != "source-a" || args["credentialId"] != "grant-a" {
			t.Fatalf("wrong binding: %v", args)
		}
		if args["repoUrl"] != payload["repoUrl"] {
			return nil, refused
		}
		return nil, nil
	}
	if err := e.validateSourceConnectionWrite(ctx, "v1:platform:package", nil, payload); err != nil {
		t.Fatal(err)
	}
	if err := e.validateSourceConnectionWrite(ctx, "v1:platform:package", payload, map[string]any{"repoUrl": "https://github.com/other/private"}); !errors.Is(err, refused) {
		t.Fatalf("forged URL accepted: %v", err)
	}
	before := calls
	if err := e.validateSourceConnectionWrite(ctx, "v1:platform:package", payload, map[string]any{"latestKnownVersion": "new"}); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("ordinary pipeline update revalidated a removed chooser entry")
	}
	if err := e.validateSourceConnectionWrite(ctx, "v1:platform:package", payload, map[string]any{"sourceConnectionId": ""}); err == nil {
		t.Fatal("binding stripped")
	}
}

func TestPackageSourceRebindingMustPreserveTheFetchOwner(t *testing.T) {
	calls := 0
	e := &MemQLEngine{builtinExecutorHandlers: map[string]builtinExecutorHandler{
		"integration.packages.validateSourceConnectionRepository": func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) { calls++; return nil, nil },
	}}
	prior := map[string]any{"ownerUserId": "v1:identity:user:alice", "sourceKind": "repo", "sourceConnectionId": "alice-source", "credentialId": "alice-grant", "repoUrl": "https://github.com/org/repo"}
	delta := map[string]any{"sourceConnectionId": "bob-source", "credentialId": "bob-grant"}
	if err := e.validateSourceConnectionWrite(rankActorCtx("bob", auth.RoleOwner), "v1:platform:package", prior, delta); err == nil {
		t.Fatal("organization editor replaced another owner's fetch identity")
	}
	if calls != 0 {
		t.Fatal("refused rebind reached provider")
	}
	if err := e.validateSourceConnectionWrite(rankActorCtx("alice", auth.RoleOwner), "v1:platform:package", prior, delta); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("own rebind was not verified")
	}
	if err := e.validateSourceConnectionWrite(rankActorCtx("bob", auth.RoleOwner), "v1:platform:package", prior, map[string]any{"latestKnownVersion": "next"}); err != nil {
		t.Fatal("ordinary authorized pipeline update was blocked", err)
	}
}

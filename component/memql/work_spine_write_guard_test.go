package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestSpineSnapshotIsNativeAdmittedAndImmutable(t *testing.T) {
	client := context.Background()
	internal := auth.ContextWithInternalOrigin(client)
	before := map[string]any{"spine": map[string]any{"version": "admitted"}}
	after := map[string]any{"spine": map[string]any{"version": "substituted"}}
	if err := validateWorkSpineImmutable(client, nil, before); err == nil {
		t.Fatal("client forged an admitted snapshot")
	}
	if err := validateWorkSpineImmutable(internal, nil, before); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{client, internal} {
		for _, changed := range []map[string]any{after, {"spine": nil}, {}} {
			if err := validateWorkSpineImmutable(ctx, before, changed); err == nil {
				t.Fatal("persisted snapshot was replaced or removed")
			}
		}
		if err := validateWorkSpineImmutable(ctx, before, map[string]any{"spine": before["spine"], "status": "running"}); err != nil {
			t.Fatal("ordinary run update refused:", err)
		}
	}
}

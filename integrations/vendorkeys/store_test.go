package vendorkeys

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/worker"
)

// A key is one person's money, so there is no way to lend one to everyone. A
// row that says `cluster` anyway (written by hand, or by a future engine) is
// read as lent to nobody.
func TestKeyFromRowNeverHonoursAClusterShare(t *testing.T) {
	key := keyFromRow(map[string]any{
		"id": "v1:router:vendorKey:k1", "ownerUserId": "v1:identity:user:alice", "vendor": "anthropic",
		"status": "active", "label": "Personal",
		"sharing": map[string]any{"mode": "cluster", "userIds": []any{"bob"}},
	})
	if key.Sharing.Mode != worker.SharingModeOwner || len(key.Sharing.UserIds) != 0 {
		t.Fatalf("sharing = %+v, want owner only", key.Sharing)
	}
	if key.Sharing.Admits(worker.NewPerson(t.Context(), "bob", func(context.Context, string) []string { return nil })) {
		t.Error("a key with a cluster share stored on it was lent to somebody")
	}
	if key.Id != "k1" || key.OwnerUserId != "alice" {
		t.Errorf("ids = %q / %q, want bare", key.Id, key.OwnerUserId)
	}

	lent := keyFromRow(map[string]any{
		"id": "k2", "ownerUserId": "alice", "vendor": "anthropic", "status": "active",
		"sharing": map[string]any{"mode": "people", "userIds": []any{"bob"}},
	})
	if lent.Sharing.Mode != worker.SharingModePeople || len(lent.Sharing.UserIds) != 1 {
		t.Fatalf("a people share was lost: %+v", lent.Sharing)
	}
}

// The sealed value and the plain key are held in a type that prints as
// nothing, so a careless log line or error cannot carry either.
func TestTheSealedValueNeverPrints(t *testing.T) {
	const text = "c2VhbGVkLXZhbHVlLXRoYXQtbXVzdC1ub3QtcHJpbnQ"
	v := sealedValue(text)
	holder := struct {
		Name  string
		Value sealedValue
	}{"k1", v}

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("a key", "value", v, "holder", holder)

	for name, got := range map[string]string{
		"%v":   fmt.Sprintf("%v", v),
		"%s":   fmt.Sprintf("%s", v),
		"%q":   fmt.Sprintf("%q", v),
		"%+v":  fmt.Sprintf("%+v", holder),
		"%#v":  fmt.Sprintf("%#v", holder),
		"err":  fmt.Errorf("could not use %v", v).Error(),
		"slog": logged.String(),
	} {
		if strings.Contains(got, text) || strings.Contains(got, text[:12]) {
			t.Errorf("%s printed the sealed value: %s", name, got)
		}
	}
	if v.reveal() != text {
		t.Error("the one deliberate read does not return the value")
	}
}

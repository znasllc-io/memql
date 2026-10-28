package azureblob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Exercise the new adapter against the actual provider protocol. The separate
// SDK fault and required-DB tests cover lost replies and ownership/accounting.
func TestAzuritePackageActiveSetConditionalLifecycle(t *testing.T) {
	u := azuriteUploader(t)
	other := azuriteUploader(t) // independent SDK client; no originating state
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	container := fmt.Sprintf("package-active-%d", time.Now().UnixNano())
	if err := u.EnsureContainer(ctx, container); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if _, err := u.client.DeleteContainer(cleanup, container, nil); err != nil {
			t.Errorf("remove owned active-set fixture: %v", err)
		}
	})
	absent, err := u.ReadPackageActiveSet(ctx, container)
	if err != nil || absent.Exists || absent.ETag != "" || len(absent.Data) != 0 {
		t.Fatalf("expected confirmed absence in the existing container: %+v %v", absent, err)
	}
	first := []byte(`{"example":"packages/example/11111111111111111111111111111111/"}`)
	created, err := u.CompareAndSwapPackageActiveSet(ctx, container, absent, first)
	if err != nil || !created.Exists || !validVerifiedETag(created.ETag) || !bytes.Equal(created.Data, first) {
		t.Fatalf("conditional create: %+v %v", created, err)
	}
	next := []byte(`{"example":"packages/example/22222222222222222222222222222222/"}`)
	replaced, err := other.CompareAndSwapPackageActiveSet(ctx, container, created, next)
	if err != nil || !replaced.Exists || !validVerifiedETag(replaced.ETag) || replaced.ETag == created.ETag || !bytes.Equal(replaced.Data, next) {
		t.Fatalf("exact replacement: %+v %v", replaced, err)
	}
	for _, stale := range []PackageActiveSetSnapshot{absent, created} {
		if _, err := u.CompareAndSwapPackageActiveSet(ctx, container, stale, first); !errors.Is(err, ErrPackageActiveSetChanged) {
			t.Fatalf("stale create/version must refuse: %v", err)
		}
		observed, err := other.ReadPackageActiveSet(ctx, container)
		if err != nil || !observed.Exists || observed.ETag != replaced.ETag || !bytes.Equal(observed.Data, next) {
			t.Fatalf("refused write changed the exact stored version: %+v %v", observed, err)
		}
	}
}

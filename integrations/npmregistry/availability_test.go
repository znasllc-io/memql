package npmregistry

import (
	"context"
	"strings"
	"testing"
)

func TestObservePublishedPackageReadsFreshBytesWithoutWriting(t *testing.T) {
	r := newRegistry(t)
	v := verified(t)
	r.stored = append([]byte(nil), v.archive...)
	got, err := r.publisher(t).Observe(t.Context(), v.expected)
	if err != nil || got.SHA256 != v.expected.SHA256 || got.Size != v.expected.Size {
		t.Fatal(got, err)
	}
	// A replacement client must check the current bytes, rather than recover
	// success from the prior availability observation or publication receipt.
	r.stored = []byte("replaced")
	if _, err := r.publisher(t).Observe(t.Context(), v.expected); err == nil {
		t.Fatal("changed published bytes were accepted")
	}
	if r.writes.Load() != 0 {
		t.Fatal("availability performed a publication")
	}
}

func TestObservePublishedPackageRefusesUnavailableAndUnboundedInputs(t *testing.T) {
	for _, mode := range []string{"missing", "wrong name", "wrong version", "invalid digest", "zero size", "excess size", "foreign download", "unauthorized", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			r := newRegistry(t)
			v := verified(t)
			r.stored = append([]byte(nil), v.archive...)
			want := v.expected
			ctx := t.Context()
			switch mode {
			case "missing":
				r.stored = nil
			case "wrong name":
				want.Name = "@other/sdk"
			case "wrong version":
				want.Version = "2.0.0"
			case "invalid digest":
				want.SHA256 = strings.Repeat("x", 64)
			case "zero size":
				want.Size = 0
			case "excess size":
				want.Size = maxArchive + 1
			case "foreign download":
				r.tarball = "https://other.invalid/archive"
			case "unauthorized":
				r.metadataCode = 401
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := r.publisher(t).Observe(ctx, want); err == nil {
				t.Fatal("invalid availability succeeded")
			}
			if r.writes.Load() != 0 {
				t.Fatal("failed observation published an archive")
			}
		})
	}
}

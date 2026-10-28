package azureblob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestClusterAssetsVerifiedCreateRetainsIntegrityAndCallerScope(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "confirmed"
		if lost {
			name = "lost reply"
		}
		t.Run(name, func(t *testing.T) {
			f := &verifiedFixture{lostCommit: lost, blocks: map[string][]byte{}}
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "original-owner", Role: "owner"})
			u := activeSetClient(t, func(r *http.Request) (*http.Response, error) {
				actual, _ := auth.AccessFromContext(r.Context())
				expected, _ := auth.AccessFromContext(ctx)
				if actual != expected {
					t.Fatal("shared accounting changed caller authority")
				}
				return f.do(r)
			})
			body := []byte("reviewed final site bytes")
			path := "sites/site-one/v123456789abc/index.html"
			got, err := u.ClusterAssetsCreateVerifiedStream(ctx, "container", path, bytes.NewReader(body), int64(len(body)), verifiedSHA(body), "text/html")
			if err != nil || got.SHA256 != verifiedSHA(body) || got.ETag != `"stored-v1"` || f.writes != 1 || f.commits != 1 {
				t.Fatalf("create: %+v %v writes=%d", got, err, f.writes)
			}
			// Another caller instance verifies existing bytes without reading its
			// input or staging another output. The same receipt pins later use.
			other := &AzureBlobUploader{client: u.client}
			readback, err := other.ClusterAssetsCreateVerifiedStream(ctx, "container", path, bytes.NewReader(nil), int64(len(body)), verifiedSHA(body), "text/html")
			if err != nil || readback != got || f.commits != 1 {
				t.Fatalf("re-entry repeated effect: %+v %v", readback, err)
			}
		})
	}
}

func TestClusterAssetsVerifiedCreateRefusesMutableAndPersonalDestinations(t *testing.T) {
	var u *AzureBlobUploader
	for _, path := range []string{"packages/active.json", "library/user/file", "objects/user/file", "backups/user/file", "packages/snapshots/" + strings.Repeat("a", 32) + "/source", "packages/builds/" + strings.Repeat("a", 32) + "/output", "sites/site-one/latest/index.html", "sites/site-one/v123456789abc/../file"} {
		if _, err := u.ClusterAssetsCreateVerifiedStream(context.Background(), "container", path, bytes.NewReader(nil), 0, verifiedSHA(nil), ""); err == nil || !strings.Contains(err.Error(), "immutable native asset") {
			t.Fatalf("unsafe shared destination %q: %v", path, err)
		}
	}
}

func TestClusterAssetsVerifiedCreateCannotReplaceDifferentBytes(t *testing.T) {
	f := &verifiedFixture{exists: true, body: []byte("old")}
	u := verifiedClient(t, f)
	_, err := u.ClusterAssetsCreateVerifiedStream(context.Background(), "container", "sites/site-one/v123456789abc/index.html", strings.NewReader("new"), 3, verifiedSHA([]byte("new")), "text/html")
	if err == nil || f.commits != 0 || f.writes != 0 || string(f.body) != "old" {
		t.Fatalf("immutable asset replaced: %v writes=%d", err, f.writes)
	}
}

func TestNativeSharedWritesRequireRetentionObservation(t *testing.T) {
	for _, operation := range []string{"active set", "verified asset"} {
		t.Run(operation, func(t *testing.T) {
			reads := 0
			u := activeSetClient(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Query().Get("comp") != "properties" || r.URL.Query().Get("restype") != "service" {
					t.Fatalf("unqualified write reached provider: %s %s", r.Method, r.URL.Path)
				}
				reads++
				return nil, errors.New("retention observation unavailable")
			})
			u.meterWrites = true
			var err error
			if operation == "active set" {
				_, err = u.CompareAndSwapPackageActiveSet(context.Background(), "container", PackageActiveSetSnapshot{}, []byte("{}"))
			} else {
				_, err = u.ClusterAssetsCreateVerifiedStream(context.Background(), "container", "sites/site-one/v123456789abc/index.html", io.NopCloser(strings.NewReader("x")), 1, verifiedSHA([]byte("x")), "text/html")
			}
			if err == nil || reads != 1 || !strings.Contains(err.Error(), "retention settings") {
				t.Fatalf("retention refusal: %v reads=%d", err, reads)
			}
		})
	}
}

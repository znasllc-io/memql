package packages

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/azureblob"
)

// pointerStager reads the pointer through the real Azure SDK from a loopback
// endpoint, so what is under test is the error the SDK really raises rather than
// one a fake chose to raise.
func pointerStager(t *testing.T, answer http.HandlerFunc) *blobStager {
	t.Helper()
	server := httptest.NewServer(answer)
	t.Cleanup(server.Close)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "AccountName=test;AccountKey="+key+";BlobEndpoint="+server.URL+"/test;")
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "memql")
	uploader, err := azureblob.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &blobStager{uploader: uploader, container: "memql"}
}

func storageAnswers(status int, errorCode string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if errorCode != "" {
			w.Header().Set("x-ms-error-code", errorCode)
		}
		w.WriteHeader(status)
	}
}

// ONLY A POINTER THAT IS NOT THERE READS AS EMPTY. A caller copies what this
// returns, adds its own domains and writes the whole document back, so an empty
// answer to a read that merely failed replaces the cluster's pointer with one
// package's domains, and the next roll drops every other package's DSL.
func TestTheActiveSetReadsEmptyOnlyWhenThePointerIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{"the blob is not there", storageAnswers(http.StatusNotFound, "BlobNotFound")},
		{"the container is not there", storageAnswers(http.StatusNotFound, "ContainerNotFound")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := pointerStager(t, tc.answer).ReadActiveSet(context.Background())
			if err != nil {
				t.Fatalf("a first deploy has no pointer and that is not a fault: %v", err)
			}
			if set == nil || len(set) != 0 {
				t.Fatalf("set = %v, want an empty map", set)
			}
		})
	}

	t.Run("a pointer is read", func(t *testing.T) {
		stager := pointerStager(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"acme":"packages/acme/1/","other":"packages/other/2/"}`)
		})
		set, err := stager.ReadActiveSet(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if set["acme"] != "packages/acme/1/" || set["other"] != "packages/other/2/" || len(set) != 2 {
			t.Fatalf("set = %v", set)
		}
	})

	for _, tc := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{"storage is busy", storageAnswers(http.StatusServiceUnavailable, "ServerBusy")},
		{"storage failed", storageAnswers(http.StatusInternalServerError, "InternalError")},
		{"the credential is refused", storageAnswers(http.StatusForbidden, "AuthorizationFailure")},
		{"a 404 that does not say the blob is missing", storageAnswers(http.StatusNotFound, "")},
		{"the pointer was retired", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-ms-meta-memql_retired_stream", "true")
			_, _ = io.WriteString(w, `{"acme":"packages/acme/1/"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := pointerStager(t, tc.answer).ReadActiveSet(context.Background())
			if err == nil {
				t.Fatalf("a read that failed answered %v and no error, which a caller writes back over the cluster's pointer", set)
			}
			if set != nil {
				t.Fatalf("a failed read handed back a set to build on: %v", set)
			}
			if !strings.Contains(err.Error(), ActiveSetPath) {
				t.Fatalf("the error must name what could not be read: %v", err)
			}
		})
	}

	t.Run("the request was cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stager := pointerStager(t, storageAnswers(http.StatusOK, ""))
		if set, err := stager.ReadActiveSet(ctx); err == nil {
			t.Fatalf("a cancelled read answered %v and no error", set)
		}
	})
}

// A DEPLOY THAT CANNOT READ THE POINTER STOPS BEFORE IT STAGES OR WRITES. It has
// no way to tell what the other packages' domains are, so moving the pointer is
// the one thing it must not do.
func TestADeployThatCannotReadThePointerMovesNothing(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.deps.Stager = unreadableStager{fakeStager: h.stager}
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}); err == nil {
		t.Fatal("a deploy that could not read the pointer reported success")
	}
	if len(h.stager.staged) != 0 || h.stager.written != 0 || h.roller.rolls != 0 || len(h.publisher.published) != 0 {
		t.Fatalf("something moved: staged=%v written=%d rolls=%d published=%v",
			h.stager.staged, h.stager.written, h.roller.rolls, h.publisher.published)
	}
}

// A ROLLBACK THAT CANNOT READ THE POINTER MOVES NOTHING EITHER. Its sites are put
// back before its pointer, so the read has to come before both: asked after, it
// fails with the sites already on the old build and the DSL still on the new one.
func TestARollbackThatCannotReadThePointerMovesNothing(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":         "v1:platform:packageDeployment:old",
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": "packages/acme/0123456789abcdef/",
		"deployables": []any{
			map[string]any{"name": "storefront", "siteId": "v1:platform:site:s1", "bundleRef": "blob://sites/s1/v1/"},
		},
	}}
	h.deps.Stager = unreadableStager{fakeStager: h.stager}
	if _, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:old",
		Actor:        mayDeployDsl(),
	}); err == nil {
		t.Fatal("a rollback that could not read the pointer reported success")
	}
	if len(h.publisher.repointed) != 0 || h.stager.written != 0 || h.roller.rolls != 0 {
		t.Fatalf("something moved: repointed=%v written=%d rolls=%d",
			h.publisher.repointed, h.stager.written, h.roller.rolls)
	}
}

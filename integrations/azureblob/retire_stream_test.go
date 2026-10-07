package azureblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func retirementClient(t *testing.T, transport policy.Transporter) (*AzureBlobUploader, string) {
	t.Helper()
	connection := os.Getenv(azuriteEnv)
	if connection == "" {
		t.Skip("MEMQL_AZURITE_TEST_CONNECTION_STRING is required for real retirement protocol tests")
	}
	c, err := azblob.NewClientFromConnectionString(connection, &azblob.ClientOptions{ClientOptions: policy.ClientOptions{
		Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := &AzureBlobUploader{client: c}
	container := "retire-" + strings.ToLower(rand.Text())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := u.EnsureContainer(ctx, container); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// This container is owned solely by this test. Removing it also removes
		// permanently leased tombstones without weakening production fences.
		if _, err := c.DeleteContainer(ctx, container, nil); err != nil {
			t.Error("fixture container cleanup", err)
		}
	})
	return u, container
}

func TestRetirementRejectsUnboundedOrMissingIdentityBeforeNetwork(t *testing.T) {
	u := &AzureBlobUploader{}
	valid := VerifiedBlob{Size: 0, SHA256: verifiedSHA(nil)}
	for _, id := range []string{"", "invalid", strings.Repeat("f", 10000), "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"} {
		if _, err := u.RetireVerifiedStream(context.Background(), "c", "o", valid, id); err == nil || strings.Contains(err.Error(), "not initialized") {
			t.Fatalf("invalid lease reached provider: %v", err)
		}
	}
}

func TestAzuriteRetirementFencesLateWritersAndClearsStaging(t *testing.T) {
	u, container := retirementClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const token = "873afe3b-4b29-42b5-8eaa-9af278f0c981"
	body := []byte("owned artifact")
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "committed"}[existing], func(t *testing.T) {
			object := rand.Text()
			receipt := VerifiedBlob{Size: int64(len(body)), SHA256: verifiedSHA(body)}
			if existing {
				var err error
				receipt, err = u.CreateVerifiedStream(ctx, container, object, bytes.NewReader(body), receipt.Size, receipt.SHA256, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := u.StageBlock(ctx, container, object, blockIDForTest(1), []byte("staging")); err != nil {
				t.Fatal(err)
			}
			got, err := u.RetireVerifiedStream(ctx, container, object, receipt, token)
			if err != nil || got.ETag == "" {
				t.Fatalf("retire: %+v %v", got, err)
			}
			if err := u.StageBlock(ctx, container, object, blockIDForTest(2), []byte("late writer")); err == nil {
				t.Fatal("late staging was admitted")
			}
			if err := u.CommitBlockList(ctx, container, object, []string{blockIDForTest(1)}, ""); err == nil {
				t.Fatal("late commit was admitted")
			}
			if _, err := u.CreateVerifiedStream(ctx, container, object, bytes.NewReader(nil), 0, verifiedSHA(nil), ""); !errors.Is(err, ErrBlobRetired) {
				t.Fatalf("adopted empty tombstone: %v", err)
			}
			if blocks, err := u.UncommittedBlocks(ctx, container, object); err != nil || len(blocks) != 0 {
				t.Fatalf("staged bytes survived: %v %v", blocks, err)
			}
			if _, err := u.DownloadWithLimit(ctx, container, object, 20); !errors.Is(err, ErrBlobRetired) {
				t.Fatalf("download accepted tombstone: %v", err)
			}
			bc, _ := u.blockClient(container, object)
			if _, err := u.DownloadStreamURL(ctx, bc.URL()); !errors.Is(err, ErrBlobRetired) {
				t.Fatalf("stream accepted tombstone: %v", err)
			}
			if _, err := u.DownloadRangeURL(ctx, bc.URL(), 0, 1); err == nil {
				t.Fatal("range accepted tombstone")
			}
			if _, err := u.RetireVerifiedStream(ctx, container, object, receipt, token); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if _, err := u.RetireVerifiedStream(ctx, container, object, receipt, "073afe3b-4b29-42b5-8eaa-9af278f0c981"); err == nil {
				t.Fatal("different retirement token adopted fence")
			}
		})
	}
}

func TestAzuriteRetirementRefusesChangedContentAndETag(t *testing.T) {
	u, container := retirementClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	body := []byte("keep these bytes")
	r, err := u.CreateVerifiedStream(ctx, container, "keep", bytes.NewReader(body), int64(len(body)), verifiedSHA(body), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []VerifiedBlob{{Size: r.Size, SHA256: verifiedSHA([]byte("wrong"))}, {Size: r.Size, SHA256: r.SHA256, ETag: `"stale"`}} {
		if _, err := u.RetireVerifiedStream(ctx, container, "keep", bad, "873afe3b-4b29-42b5-8eaa-9af278f0c981"); err == nil {
			t.Fatal("mismatched retirement succeeded")
		}
	}
	if got, err := u.VerifyStream(ctx, container, "keep", r.Size, r.SHA256); err != nil || got != r {
		t.Fatalf("changed unrelated bytes: %+v %v", got, err)
	}
}

func TestAzuriteRetirementReconcilesLostResponsesAndLeaseGap(t *testing.T) {
	for _, phase := range []string{"tombstone", "lease", "clear"} {
		t.Run(phase, func(t *testing.T) {
			ordinary, container := retirementClient(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			lost, stagedInGap := false, false
			transport := downloadTransport(func(r *http.Request) (*http.Response, error) {
				isLease := r.Method == http.MethodPut && r.URL.Query().Get("comp") == "lease"
				if isLease && !stagedInGap {
					// Stage after the first tombstone but before the lease. The
					// under-lease empty write must remove this exact gap's bytes.
					if err := ordinary.StageBlock(ctx, container, "lost", blockIDForTest(8), []byte("gap bytes")); err != nil {
						return nil, err
					}
					stagedInGap = true
				}
				resp, err := http.DefaultTransport.RoundTrip(r)
				// The generated SDK writes this header under a lowercase map key;
				// Header.Get canonicalizes its lookup and misses that raw spelling.
				leased := false
				for name, values := range r.Header {
					leased = leased || (strings.EqualFold(name, "x-ms-lease-id") && len(values) > 0 && values[0] != "")
				}
				match := (phase == "lease" && isLease) || (r.Method == http.MethodPut && r.URL.Query().Get("comp") == "blocklist" && ((phase == "tombstone" && !leased) || (phase == "clear" && leased)))
				if err == nil && resp.StatusCode < 300 && match && !lost {
					lost = true
					_ = resp.Body.Close()
					return nil, errors.New("lost successful retirement reply")
				}
				return resp, err
			})
			c, err := azblob.NewClientFromConnectionString(os.Getenv(azuriteEnv), &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}}})
			if err != nil {
				t.Fatal(err)
			}
			faulty := &AzureBlobUploader{client: c}
			r := VerifiedBlob{Size: 4, SHA256: verifiedSHA([]byte("data"))}
			_, err = faulty.RetireVerifiedStream(ctx, container, "lost", r, "873afe3b-4b29-42b5-8eaa-9af278f0c981")
			if err != nil && !errors.Is(err, ErrBlobRetirementUncertain) {
				t.Fatal(err)
			}
			if !lost {
				t.Fatalf("lost-reply fixture never fired: %v", err)
			}
			if _, err := ordinary.RetireVerifiedStream(ctx, container, "lost", r, "873afe3b-4b29-42b5-8eaa-9af278f0c981"); err != nil {
				t.Fatalf("replacement recovery: %v", err)
			}
			if blocks, err := ordinary.UncommittedBlocks(ctx, container, "lost"); err != nil || len(blocks) != 0 {
				t.Fatalf("gap staging survived: %v %v", blocks, err)
			}
		})
	}
}

type pausedRetirementReader struct{ started, proceed chan struct{} }

func (r pausedRetirementReader) Read(p []byte) (int, error) {
	close(r.started)
	<-r.proceed
	return copy(p, "data"), io.EOF
}

func TestAzuriteRetirementStopsAnAlreadyAdmittedUpload(t *testing.T) {
	u, container := retirementClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := pausedRetirementReader{make(chan struct{}), make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		_, err := u.CreateVerifiedStream(ctx, container, "paused", r, 4, verifiedSHA([]byte("data")), "")
		finished <- err
	}()
	select {
	case <-r.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err := u.RetireVerifiedStream(ctx, container, "paused", VerifiedBlob{Size: 4, SHA256: verifiedSHA([]byte("data"))}, "873afe3b-4b29-42b5-8eaa-9af278f0c981")
	close(r.proceed)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err == nil {
		t.Fatal("in-flight producer resurrected retired object")
	}
}

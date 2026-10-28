package packages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestContinuationRowVersionKeepsNativeAndWirePrecision(t *testing.T) {
	want := time.Date(2026, 10, 25, 12, 34, 56, 123456000, time.UTC)
	for _, value := range []any{timestamppb.New(want), want, want.Format(time.RFC3339Nano)} {
		got, err := continuationRowVersion(map[string]any{"createdAt": value})
		if err != nil || !got.Equal(want) {
			t.Fatalf("native/wire version %T lost precision: %v %v", value, got, err)
		}
	}
	for _, value := range []any{nil, (*timestamppb.Timestamp)(nil), time.Time{}, timestamppb.New(time.Time{}), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "", "not-a-time", "2026-10-25", map[string]any{"seconds": 42}, &timestamppb.Timestamp{Seconds: 253402300800}} {
		if _, err := continuationRowVersion(map[string]any{"createdAt": value}); err == nil {
			t.Fatalf("accepted invalid row version %T", value)
		}
	}
}

type preparedPublicationEngine struct {
	recordingEngine
	writes    int
	roleState *atomic.Int32
	roleReads int
}

func (e *preparedPublicationEngine) OrganizationCapable(ctx context.Context, _ string, verb, resource string) bool {
	ac, ok := auth.AccessFromContext(ctx)
	return ok && auth.CapableFor(ctx, auth.Subject{Role: ac.Role, UserId: ac.UserId}, verb, resource)
}

type publicationGrantSource struct{ revoked atomic.Bool }

func (s *publicationGrantSource) ActiveGrantsForUser(context.Context, string) ([]auth.Grant, error) {
	effect := auth.GrantAllow
	if s.revoked.Load() {
		effect = auth.GrantDeny
	}
	return []auth.Grant{
		{Verb: auth.VerbUpdate, Resource: auth.ResourceData, Effect: effect},
		{Verb: auth.VerbExecute, Resource: "app:deployables/deploy", Effect: effect},
	}, nil
}
func (*publicationGrantSource) ActiveGrantsForGroups(context.Context, []string) ([]auth.Grant, error) {
	return nil, nil
}
func (e *preparedPublicationEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "query userByIdSystem(") {
		e.roleReads++
		if !memql.FreshReadFromContext(ctx) || !strings.Contains(q, `userId: "actor"`) {
			return nil, fmt.Errorf("requester role read was not exact/fresh")
		}
		switch e.roleState.Load() {
		case 1:
			return &memql.ExecuteResult{Bundle: asBundle([]map[string]any{{"role": "reader"}})}, nil
		case 2:
			return &memql.ExecuteResult{}, nil
		case 3:
			return nil, fmt.Errorf("fixture role read unavailable")
		default:
			return &memql.ExecuteResult{Bundle: asBundle([]map[string]any{{"role": "owner"}})}, nil
		}
	}
	if strings.HasPrefix(q, "mutation ") {
		ac, ok := auth.AccessFromContext(ctx)
		if !ok || ac.UserId != "actor" || ac.Role != auth.RoleOwner || !memql.HasPackagePublicationFence(ctx) || !memql.FreshReadFromContext(ctx) {
			return nil, fmt.Errorf("publication lost original requester or native freshness/fence")
		}
		e.writes++
	}
	return e.recordingEngine.Execute(ctx, q)
}

// Actual production PublishPrepared and actual Azure SDK receipt stream; only
// the provider and final graph write are offline fixtures. The two-replica DB
// suite separately proves the real site/receipt transaction.
func TestPreparedPublicationRechecksPointerAfterActualReceiptEOF(t *testing.T) {
	for _, fault := range []string{"none", "pointer-during-stream", "corrupt-output", "role-revoked-during-stream", "requester-missing-after-stream", "role-read-failed-after-stream", "grant-revoked-during-stream"} {
		t.Run(fault, func(t *testing.T) {
			grants := &publicationGrantSource{}
			previousGrants := auth.InstalledGrantSource()
			auth.SetGrantSource(grants)
			t.Cleanup(func() { auth.SetGrantSource(previousGrants) })
			data := []byte("retained index")
			sum := sha256.Sum256(data)
			version := "v111111111111"
			activeRaw := []byte(`{"acme":"packages/acme/selected/"}`)
			activeTag := `"selected"`
			var roleState atomic.Int32
			var calls []string
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.URL.Path)
				if r.Method != http.MethodGet {
					t.Error("verification attempted provider write")
					w.WriteHeader(405)
					return
				}
				switch r.URL.Path {
				case "/test/memql/sites/site-one/" + version + "/index.html":
					if r.Header.Get("If-Match") != `"retained"` {
						t.Error("asset was not ETag bound")
					}
					switch fault {
					case "role-revoked-during-stream":
						roleState.Store(1)
					case "requester-missing-after-stream":
						roleState.Store(2)
					case "role-read-failed-after-stream":
						roleState.Store(3)
					case "grant-revoked-during-stream":
						grants.revoked.Store(true)
					}
					body := bytes.Clone(data)
					if fault == "corrupt-output" {
						body[0] ^= 1
					}
					w.Header().Set("ETag", `"retained"`)
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					_, _ = w.Write(body)
					if fault == "pointer-during-stream" {
						activeTag = `"replacement"`
					}
				case "/test/memql/packages/active.json":
					w.Header().Set("ETag", activeTag)
					w.Header().Set("Content-Length", strconv.Itoa(len(activeRaw)))
					_, _ = w.Write(activeRaw)
				default:
					t.Error("unexpected provider route")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "AccountName=test;AccountKey="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))+";BlobEndpoint="+server.URL+"/test;")
			uploader, err := azureblob.New(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			engine := &preparedPublicationEngine{roleState: &roleState, recordingEngine: recordingEngine{rows: map[string][]map[string]any{"query siteById": {{"id": "site-one", "accountId": "account"}}}}}
			publisher := &enginePublisher{engine: engine, store: &store{engine: engine}, uploader: uploader, container: "memql"}
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "actor", Role: auth.RoleOwner})
			ctx = auth.ContextWithGrantMemo(ctx)
			if !engine.OrganizationCapable(ctx, "account", auth.VerbExecute, "app:deployables/deploy") {
				t.Fatal("initial publication grant did not admit caller")
			}
			ctx = memql.ContextWithPackagePublicationFence(ctx, "deployment", strings.Repeat("1", 64), "lease", "site-one/serving")
			selected := PreparedSiteBundle{SiteID: "site-one", Target: edge.TargetServing, Version: version, BundleRef: "blob://sites/site-one/" + version + "/", Files: []PreparedSiteFile{{Name: "index.html", Receipt: azureblob.VerifiedBlob{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ETag: `"retained"`}}}}
			expected := ActiveSetVersion{Exists: true, ETag: `"selected"`, Data: activeRaw, Domains: map[string]string{"acme": "packages/acme/selected/"}}
			_, err = publisher.PublishPrepared(ctx, selected, expected)
			if fault == "grant-revoked-during-stream" && !engine.OrganizationCapable(ctx, "account", auth.VerbExecute, "app:deployables/deploy") {
				t.Fatal("fixture did not retain the original request's stale grant memo")
			}
			if fault == "none" {
				if err != nil || engine.writes != 1 {
					t.Fatalf("valid publish: writes=%d %v", engine.writes, err)
				}
			} else if err == nil || engine.writes != 0 {
				t.Fatalf("unverified write: writes=%d %v", engine.writes, err)
			}
			roleReads := 1
			if fault == "pointer-during-stream" || fault == "corrupt-output" {
				roleReads = 0
			}
			if engine.roleReads != roleReads {
				t.Fatalf("fresh role read count=%d want=%d", engine.roleReads, roleReads)
			}
			want := 2
			if fault == "corrupt-output" {
				want = 1
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != want {
				t.Fatal(fmt.Sprintf("provider calls %v want %d", calls, want))
			}
		})
	}
}

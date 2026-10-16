package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

type uploadRoleValidator struct{}

func (uploadRoleValidator) ValidateRoleTest(context.Context, string, string, auth.Role) error {
	return nil
}

type authorityAnalyzer struct {
	contexts chan context.Context
	release  chan struct{}
}

func (a *authorityAnalyzer) AnalyzeFile(ctx context.Context, _ LibraryAnalysisRequest) {
	a.contexts <- ctx
	<-a.release
}

func TestViewerUploadAnalysisRetainsSessionAfterRequestEnds(t *testing.T) {
	auth.SetRoleTestValidator(uploadRoleValidator{})
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
	for _, forwarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "forwarded"}[forwarded], func(t *testing.T) {
			persona := "v1:identity:user:role-test-upload"
			expires := time.Now().Add(time.Minute).Truncate(time.Second)
			claims := map[string]any{"sub": persona, "role": "reader", "class": auth.RoleTestClass, "role_test_session": "upload-session", "exp": expires.Unix()}
			ac := &auth.AccessContext{UserId: persona, Role: auth.RoleReader, RoleTestSessionID: "upload-session"}
			ctx := auth.ContextWithAccess(auth.ContextWithToken(auth.ContextWithClaims(context.Background(), claims), auth.BuildTokenInfo(claims)), ac)
			if forwarded {
				a, _ := auth.ForwardedAuthorityForUser(ac, auth.RoleTestClass, "", expires, time.Now())
				ctx = auth.BindForwardedContext(context.Background(), a.Principal().Claims, ac, a)
			}
			request, cancel := context.WithCancel(ctx)
			defer cancel()
			analyzer := &authorityAnalyzer{contexts: make(chan context.Context, 1), release: make(chan struct{})}
			defer close(analyzer.release)
			blob := newFakeBlob()
			h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: newFakeLibraryStore(), Analyzer: analyzer, PromotionWait: 50 * time.Millisecond})
			body, ct := uploadBody(t, "viewer.md", "text/markdown", []byte("# Viewer\n"), nil)
			req := httptest.NewRequest(http.MethodPost, "/artifacts", body).WithContext(request)
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
			}
			cancel()
			select {
			case analysis := <-analyzer.contexts:
				if analysis.Err() != nil {
					t.Fatal("request cancellation reached analysis")
				}
				got, _ := auth.AccessFromContext(analysis)
				if got != ac {
					t.Fatalf("analysis lost Viewer authority: %+v", got)
				}
				if deadline, ok := analysis.Deadline(); !ok || !deadline.Equal(expires) {
					t.Fatal("analysis lost test expiry")
				}
				if err := auth.ValidateRoleTestContext(analysis); err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upload never started analysis")
			}
		})
	}
}

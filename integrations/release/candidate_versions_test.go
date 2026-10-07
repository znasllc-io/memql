package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCandidateVersionReadsExactCommitAndConfiguredFile(t *testing.T) {
	c := candidateTestManifest().Components[0]
	f := newFakeGitHub(t, nil, "different-main").withVersionFile("0.25.0\n")
	f.files["clients/editor/package.json"] = `{"name":"fixture","version":"0.25.0"}`
	for _, tc := range []struct{ path, field string }{{"VERSION", ""}, {"clients/editor/package.json", "version"}} {
		r := candidateVersionReader{client: NewClient().WithBaseURL(f.server.URL), sources: map[string]candidateVersionSource{
			c.Name: {Component: c.Name, Repository: c.Repository, Path: tc.path, JSONField: tc.field},
		}}
		if err := r.check(ownerCtx(), c); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		last := f.fileReads[len(f.fileReads)-1]
		f.mu.Unlock()
		if last != tc.path+"@"+c.Commit {
			t.Fatal("version came from a moving ref or another file", last)
		}
		wrong := c
		wrong.Version = "0.26.0"
		if err := r.check(ownerCtx(), wrong); err == nil {
			t.Fatal("wrong source version accepted")
		}
		wrong.Commit = "main"
		if err := r.check(ownerCtx(), wrong); err == nil {
			t.Fatal("moving ref accepted")
		}
		wrong = c
		wrong.Repository = "acme/other"
		if err := r.check(ownerCtx(), wrong); err == nil {
			t.Fatal("unconfigured source accepted")
		}
	}
}

func TestCandidateVersionCredentialDoesNotFallBackToPlaintext(t *testing.T) {
	c := candidateTestManifest().Components[0]
	source := candidateVersionSource{Component: c.Name, Repository: c.Repository, Path: "VERSION", CredentialSecret: "SOURCE_TOKEN"}
	f := newFakeGitHub(t, nil, c.Commit).withVersionFile("0.25.0\n")
	r := candidateVersionReader{client: NewClient().WithBaseURL(f.server.URL), sources: map[string]candidateVersionSource{c.Name: source}, resolver: resolver{
		systemSecret: func(context.Context, string) (string, error) {
			return "", errors.New("secret backend failed with confidential text")
		},
		systemVariable: func(context.Context, string) (string, error) { t.Fatal("read plaintext credential"); return "", nil },
		env:            func(string) string { t.Fatal("read environment credential"); return "" },
		appToken: func(context.Context, repoRef) (string, error) {
			t.Fatal("bypassed configured credential reference")
			return "", nil
		},
	}}
	if err := r.check(ownerCtx(), c); err == nil || strings.Contains(err.Error(), "confidential") {
		t.Fatal("credential refusal disclosed backend data", err)
	}
	if len(f.fileReads) != 0 {
		t.Fatal("credential failure reached source")
	}
	if err := r.check(context.Background(), c); err == nil {
		t.Fatal("unresolved actor read source")
	}
}

func TestCandidateVersionAnonymousReadOmitsAuthorization(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called = true
		if req.Header.Get("Authorization") != "" {
			t.Error("anonymous version read sent an empty bearer header")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := candidateTestManifest().Components[0]
	r := candidateVersionReader{client: NewClient().WithBaseURL(srv.URL), sources: map[string]candidateVersionSource{c.Name: {Component: c.Name, Repository: c.Repository, Path: "VERSION"}}}
	if err := r.check(ownerCtx(), c); err == nil || !called {
		t.Fatal("missing version file was accepted or not read", err)
	}
}

func TestCandidateVersionFileRefusesAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`{"version":"0.25.0","version":"9.0.0"}`, `{"version":25}`, `{"version":null}`, `{"other":"0.25.0"}`, `{"version":"0.25.0"} {}`, `[{"version":"0.25.0"}]`, `{"version":"0.25.0"`, strings.Repeat(" ", 256<<10) + `{"version":"0.25.0"}`} {
		if _, err := candidateFileVersion(raw, "version"); err == nil {
			t.Fatal("invalid version source accepted")
		}
	}
	for _, path := range []string{"../VERSION", "x/../VERSION", "./VERSION", "/VERSION", "VERSION?ref=main", "VERSION#fragment", "x//VERSION", "x\\VERSION", ".", ".."} {
		if err := (candidateVersionSource{Component: "engine", Repository: "acme/engine", Path: path}).validate(); err == nil {
			t.Fatal("unsafe source path", path)
		}
	}
}

func TestCandidateVersionRuleSnapshotDoesNotFollowReload(t *testing.T) {
	c := candidateTestManifest().Components[0]
	f := newFakeGitHub(t, nil, c.Commit).withVersionFile("0.25.0\n")
	r := candidateVersionReader{client: NewClient().WithBaseURL(f.server.URL), sources: map[string]candidateVersionSource{c.Name: {Component: c.Name, Repository: c.Repository, Path: "VERSION"}}}
	frozen, rules, err := r.freezeFor([]pl.ReleaseComponent{c})
	if err != nil {
		t.Fatal(err)
	}
	r.sources[c.Name] = candidateVersionSource{Component: c.Name, Repository: "acme/other", Path: "elsewhere"}
	if err := frozen.check(ownerCtx(), c); err != nil || rules[0].Path != "VERSION" {
		t.Fatal("reload changed owned version rules", err)
	}
}

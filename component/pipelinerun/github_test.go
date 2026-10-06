package pipelinerun

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// github_test.go -- the production GitHub port and the filtered tree.

type tarMember struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// gitHubTarball is what the tarball API returns: a PAX global header, then
// one synthesized top-level directory holding the tree.
func gitHubTarball(t *testing.T, top string, members []tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(h *tar.Header, body string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": shaA}}, "")
	if top != "" {
		write(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	}
	for _, m := range members {
		name := m.name
		if top != "" {
			name = top + "/" + m.name
		}
		flag := m.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		h := &tar.Header{Name: name, Typeflag: flag, Mode: 0o644, Linkname: m.linkname}
		if flag == tar.TypeReg {
			h.Size = int64(len(m.body))
		}
		write(h, m.body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func goFilesAndManifest(p string) bool {
	return p == "memql-package.yaml" || p == "go.mod" || p == "go.work" || strings.HasSuffix(p, ".go")
}

func TestReadTreeKeepsWhatIsAskedForWithoutGitHubsTopDirectory(t *testing.T) {
	archive := gitHubTarball(t, "acme-shop-1111111", []tarMember{
		{name: "memql-package.yaml", body: "formatVersion: 1"},
		{name: "go.mod", body: "module acme.test/shop"},
		{name: "a/", typeflag: tar.TypeDir},
		{name: "a/a.go", body: "package a"},
		{name: "a/a_test.go", body: "package a"},
		{name: "README.md", body: "# not kept"},
		{name: "clients/app.ts", body: "not kept"},
		{name: "a/link.go", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	tree, err := ReadTree(bytes.NewReader(archive), goFilesAndManifest, 1<<20)
	if err != nil {
		t.Fatalf("ReadTree: %v", err)
	}
	var kept []string
	if err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			kept = append(kept, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a/a.go", "a/a_test.go", "go.mod", "memql-package.yaml"}
	if strings.Join(kept, ",") != strings.Join(want, ",") {
		t.Errorf("kept %v, want %v (top directory stripped, symlink dropped, the rest filtered)", kept, want)
	}
	body, err := fs.ReadFile(tree, "go.mod")
	if err != nil || string(body) != "module acme.test/shop" {
		t.Errorf("go.mod = %q, %v", body, err)
	}
}

func TestReadTreeOfARootRelativeArchiveKeepsItsPaths(t *testing.T) {
	archive := gitHubTarball(t, "", []tarMember{
		{name: "memql-package.yaml", body: "x"},
		{name: "cmd/main.go", body: "package main"},
	})
	tree, err := ReadTree(bytes.NewReader(archive), goFilesAndManifest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"memql-package.yaml", "cmd/main.go"} {
		if _, err := fs.Stat(tree, p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestReadTreeRefusesWhatItMustNotRead(t *testing.T) {
	big := strings.Repeat("x", 64)
	if _, err := ReadTree(bytes.NewReader(gitHubTarball(t, "top", []tarMember{
		{name: "a.go", body: big}, {name: "b.go", body: big},
	})), goFilesAndManifest, 100); !errors.Is(err, ErrTreeTooLarge) {
		t.Errorf("past the cap: %v, want ErrTreeTooLarge", err)
	}
	// What is NOT kept does not count against the cap.
	if _, err := ReadTree(bytes.NewReader(gitHubTarball(t, "top", []tarMember{
		{name: "huge.bin", body: strings.Repeat("y", 4096)}, {name: "a.go", body: "package a"},
	})), goFilesAndManifest, 100); err != nil {
		t.Errorf("an unkept file counted against the cap: %v", err)
	}
	if _, err := ReadTree(bytes.NewReader(gitHubTarball(t, "", []tarMember{
		{name: "../escape.go", body: "package x"},
	})), goFilesAndManifest, 1<<20); err == nil {
		t.Errorf("a member climbing out of the tree must be refused")
	}
	if _, err := ReadTree(strings.NewReader("not gzip"), goFilesAndManifest, 1<<20); err == nil {
		t.Errorf("a body that is not gzip must be refused")
	}
}

// fakePackages is the Deployables half the port borrows.
type fakePackages struct {
	client *githubapp.Client
	calls  []string
}

func (f *fakePackages) GitHub() *githubapp.Client { return f.client }

func (f *fakePackages) InstallationToken(_ context.Context, credentialID, ownerUserID, owner, repo string) (string, int64, error) {
	f.calls = append(f.calls, credentialID+"|"+ownerUserID+"|"+owner+"|"+repo)
	return "ghs_t", 42, nil
}

func TestThePortMintsThroughTheGrantAndAsksForTheClientPerCall(t *testing.T) {
	pkgs := &fakePackages{}
	port := NewGitHub(pkgs)

	token, inst, err := port.InstallationToken(context.Background(), credID, "v1:identity:user:u1", "acme/shop")
	if err != nil || token != "ghs_t" || inst != 42 {
		t.Fatalf("token %q inst %d err %v", token, inst, err)
	}
	if len(pkgs.calls) != 1 || pkgs.calls[0] != credID+"|v1:identity:user:u1|acme|shop" {
		t.Errorf("the grant path is asked for owner and repo split: %v", pkgs.calls)
	}
	if _, _, err := port.InstallationToken(context.Background(), credID, "u", "not-a-repository"); err == nil {
		t.Errorf("a repository that is not owner/name is refused")
	}

	// No client yet: every call answers not-configured rather than panicking.
	if port.Configured() {
		t.Errorf("no client is not configured")
	}
	if _, err := port.CreateCheckRun(context.Background(), "t", "acme/shop", githubapp.CheckRun{}); !errors.Is(err, githubapp.ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}

	// The client installed later is the one the next call uses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/tarball/"+shaA || r.Header.Get("Authorization") != "Bearer ghs_t" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(gitHubTarball(t, "acme-shop-"+shaA[:7], []tarMember{{name: "memql-package.yaml", body: "name: shop"}}))
	}))
	defer srv.Close()
	pkgs.client = githubapp.New(githubapp.Config{}, githubapp.WithAPIBase(srv.URL), githubapp.WithHTTPClient(srv.Client()))
	tree, err := port.Tree(context.Background(), "ghs_t", "acme/shop", shaA, func(p string) bool { return p == "memql-package.yaml" }, 1<<20)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if body, err := fs.ReadFile(tree, "memql-package.yaml"); err != nil || string(body) != "name: shop" {
		t.Errorf("manifest = %q, %v", body, err)
	}
}

// The port reads ONE pull request's head by its number, end to end over HTTP:
// GET /repos/{owner}/{repo}/pulls/{number} under the bearer it is handed,
// "owner/name" split for the client, the reply decoded as the list decodes an
// entry -- a deleted head repository included.
func TestThePortReadsOnePullRequestsHeadByItsNumber(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer ghs_t" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/shop/pulls/42":
			_, _ = w.Write([]byte(`{"number":42,"state":"open","title":"Show the cart count","head":{"sha":"` + shaC +
				`","ref":"cart-badge","repo":{"full_name":"acme/shop"}},"base":{"sha":"` + shaBase + `","ref":"main"}}`))
		case "/repos/acme/shop/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"closed","title":"Gone","head":{"sha":"` + shaA + `","ref":"x","repo":null},"base":{"sha":"` + shaBase + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	port := NewGitHub(&fakePackages{client: githubapp.New(githubapp.Config{}, githubapp.WithAPIBase(srv.URL), githubapp.WithHTTPClient(srv.Client()))})

	head, err := port.PullRequestHead(context.Background(), "ghs_t", "acme/shop", 42)
	if err != nil {
		t.Fatalf("PullRequestHead: %v", err)
	}
	if want := (githubapp.PullRequestHead{Number: 42, Title: "Show the cart count", HeadSHA: shaC, HeadRef: "cart-badge", HeadRepository: "acme/shop", BaseSHA: shaBase}); head != want {
		t.Errorf("head = %+v, want %+v", head, want)
	}
	if deleted, err := port.PullRequestHead(context.Background(), "ghs_t", "acme/shop", 7); err != nil || deleted.HeadRepository != "" || deleted.HeadSHA != shaA {
		t.Errorf("a deleted head repository reads empty: %+v %v", deleted, err)
	}
	if _, err := port.PullRequestHead(context.Background(), "ghs_t", "acme/shop", 8); githubapp.StatusOf(err) != http.StatusNotFound {
		t.Errorf("an unknown number is GitHub's 404: %v", err)
	}
	if _, err := port.PullRequestHead(context.Background(), "ghs_t", "not-a-repository", 42); err == nil {
		t.Errorf("a repository that is not owner/name is refused")
	}
	if want := []string{"GET /repos/acme/shop/pulls/42", "GET /repos/acme/shop/pulls/7", "GET /repos/acme/shop/pulls/8"}; strings.Join(asked, "|") != strings.Join(want, "|") {
		t.Errorf("asked %v, want %v", asked, want)
	}
}

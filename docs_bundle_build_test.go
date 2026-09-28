package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cmd/docs-gen/bundle"
	"github.com/znasllc-io/memql/core/docsmd"
)

// The docs bundle is built on every pull request (memql#5717).
//
// The bundle used to run for the first time at release: a change that broke it
// merged green, and the release lane found out -- or, from v0.20.22 to
// v0.23.5, did not, and memql.io served stale docs for three weeks. This test
// builds the real docs/public into a temporary directory the way the release
// lane does, with two stand-ins: a fixed dater in place of git history (the
// pull-request checkout is shallow) and a stub for the generated concept
// catalog (rendering it loads the whole DSL; its links resolve by path). Then
// it reads what was written, not what the code meant to write.

var bundleTestTime = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

type fixedDater struct{}

func (fixedDater) LastUpdated(string) (time.Time, error) {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), nil
}

func buildTestBundle(t *testing.T, out, tarball string, check bool) (*bundle.Result, error) {
	t.Helper()
	allow, err := bundle.LoadAllowlist(filepath.FromSlash(bundle.DefaultAllowlist))
	if err != nil {
		t.Fatalf("the boundary allowlist does not parse: %v", err)
	}
	allow.Path = bundle.DefaultAllowlist
	return bundle.Build(bundle.Options{
		RepoRoot:  ".",
		Reference: boundaryReference(),
		Allow:     allow,
		Check:     check,
		Meta: bundle.Meta{
			Version: "1.2.3", Tag: "v1.2.3", Commit: strings.Repeat("0", 40),
			ReleasedAt: bundleTestTime, SiteURL: "https://memql.io",
		},
		Dater:   fixedDater{},
		Out:     out,
		Tarball: tarball,
	})
}

func TestDocsBundleBuild(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "tree")
	tgz := filepath.Join(dir, "docs-1.2.3.tgz")
	res, err := buildTestBundle(t, out, tgz, false)
	if err != nil {
		for _, v := range res.Violations {
			t.Log(v.String())
		}
		t.Fatalf("the bundle does not build: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(out, bundle.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var man bundle.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest.json does not parse: %v", err)
	}
	if man.Schema != bundle.ManifestSchema || man.Version != "1.2.3" || man.Home != bundle.HomePath {
		t.Errorf("manifest header: schema %d, version %q, home %q", man.Schema, man.Version, man.Home)
	}
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	if _, present := generic["engineVersion"]; present {
		t.Error("manifest carries engineVersion, the field that said 0.15.0 on bundle 0.21.25 (memql#5714)")
	}

	// pageCount is the selection, and every selected page is in the tree.
	var onDisk []string
	walkErr := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(out, p)
			onDisk = append(onDisk, filepath.ToSlash(rel))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if man.PageCount != res.Selected || man.PageCount != len(man.Pages) || man.PageCount != len(onDisk) || man.PageCount == 0 {
		t.Errorf("pageCount %d, selected %d, manifest pages %d, pages on disk %d: all four must agree",
			man.PageCount, res.Selected, len(man.Pages), len(onDisk))
	}

	routes := map[string]bool{}
	for _, p := range man.Pages {
		routes[p.URL] = true
	}
	llms := readString(t, filepath.Join(out, bundle.LLMSFile))
	full := readString(t, filepath.Join(out, bundle.LLMSFullFile))
	sitemap := readString(t, filepath.Join(out, bundle.SitemapFile))
	for _, p := range man.Pages {
		abs := "https://memql.io" + p.URL
		if !strings.Contains(llms, "]("+abs+")") {
			t.Errorf("llms.txt does not list %s", p.Path)
		}
		if !strings.Contains(full, "\nurl: "+abs+"\n") {
			t.Errorf("llms-full.txt does not carry %s", p.Path)
		}
		if !strings.Contains(sitemap, "<loc>"+abs+"</loc>") {
			t.Errorf("the sitemap does not list %s", p.Path)
		}

		page := readString(t, filepath.Join(out, filepath.FromSlash(p.Path)))
		// Every link is a published route or leaves the site entirely; no
		// relative path survives, so none can 404 or point into the repo.
		for _, l := range docsmd.Links(page) {
			if docsmd.IsExternal(l.Target) {
				continue
			}
			if !routes[l.Path()] {
				t.Errorf("%s:%d: link %q is neither a published route nor an absolute URL", p.Path, l.Line, l.Target)
			}
		}
		// No HTML comment outside code: stripping again changes nothing.
		if docsmd.StripHTMLComments(page) != page {
			t.Errorf("%s: an HTML comment outside code reached the bundle, where the site prints it as text", p.Path)
		}
	}
	if got := readString(t, filepath.Join(out, bundle.VersionFile)); got != "1.2.3\n" {
		t.Errorf("memql-docs-version = %q, want the bare version and a newline", got)
	}

	// The tarball holds the same tree and is the same bytes on a second build.
	again := filepath.Join(dir, "again.tgz")
	if _, err := buildTestBundle(t, "", again, false); err != nil {
		t.Fatal(err)
	}
	first, second := readBytesFile(t, tgz), readBytesFile(t, again)
	if !bytes.Equal(first, second) {
		t.Error("two builds of one tree produced different tarballs")
	}
	names := tarNames(t, first)
	for _, want := range []string{"./manifest.json", "./memql-docs-version", "./llms.txt", "./llms-full.txt", "./sitemap-docs.xml", "./" + bundle.HomePath} {
		if !names[want] {
			t.Errorf("the tarball has no %s", want)
		}
	}
}

func TestDocsBundleCheckWritesNothing(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "tree")
	tgz := filepath.Join(dir, "docs-1.2.3.tgz")
	res, err := buildTestBundle(t, out, tgz, true)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Manifest != nil {
		t.Error("check mode built a manifest")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("check mode wrote %d entries into %s", len(entries), dir)
	}
}

func readString(t *testing.T, path string) string { return string(readBytesFile(t, path)) }

func readBytesFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tarNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
}

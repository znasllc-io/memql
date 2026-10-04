package pipelinesteps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// extractTestEntry is one tar entry for a fixture archive. A zero typeflag is
// a regular file holding body.
type extractTestEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	devmajor int64
	devminor int64
	pax      map[string]string
}

// extractTestTgz builds the tar.gz the step wrapper's `tar -czf -` would
// print, entry by entry, with archive/tar -- which writes whatever name it is
// handed, so a fixture can carry the names a hostile frame would.
func extractTestTgz(t *testing.T, entries []extractTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typeflag
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typ,
			Linkname: e.linkname,
			Devmajor: e.devmajor,
			Devminor: e.devminor,
			Mode:     0o644,
		}
		if typ == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Name: e.name, Typeflag: typ, PAXRecords: e.pax}
		}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("fixture header %q: %v", e.name, err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("fixture body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("fixture tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("fixture gzip: %v", err)
	}
	return buf.Bytes()
}

// extractTestPaths is the files' paths, in the order returned.
func extractTestPaths(files []ArtifactFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// extractTestSkippedNames is the skipped entries' names, in archive order.
func extractTestSkippedNames(e *SkippedEntriesError) []string {
	out := make([]string, 0, len(e.Entries))
	for _, s := range e.Entries {
		out = append(out, s.Name)
	}
	return out
}

// extractTestBody is one returned file's bytes, by path.
func extractTestBody(t *testing.T, files []ArtifactFile, path string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return string(f.Bytes)
		}
	}
	t.Fatalf("no file %q among %v", path, extractTestPaths(files))
	return ""
}

// The traversal fixture: two regular files under the declared directory, and
// one of each entry a frame must never turn into a file -- a parent-escaping
// name, an absolute name, a ".." that happens to land back inside (no ".."
// at all is the rule, not "no .. that escapes"), a symbolic link and a
// character device.
func extractTestTraversalArchive(t *testing.T) []byte {
	return extractTestTgz(t, []extractTestEntry{
		{name: "dist/report.xml", body: "<testsuite tests=\"3\"/>"},
		{name: "../x", body: "escaped the working copy"},
		{name: "/etc/passwd", body: "root:x:0:0:root:/root:/bin/sh"},
		{name: "dist/../dist/sneaky.txt", body: "back inside by way of .."},
		{name: "dist/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "dist/null", typeflag: tar.TypeChar, devmajor: 1, devminor: 3},
		{name: "dist/sub/ok.txt", body: "ok"},
	})
}

func TestExtractArtifactsRefusesTraversal(t *testing.T) {
	assertRefused := func(t *testing.T, files []ArtifactFile, missing []string, err error) {
		t.Helper()
		// The regular files come back, sorted, with their bytes.
		if got, want := extractTestPaths(files), []string{"dist/report.xml", "dist/sub/ok.txt"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
		if got := extractTestBody(t, files, "dist/report.xml"); got != "<testsuite tests=\"3\"/>" {
			t.Errorf("dist/report.xml = %q", got)
		}
		if got := extractTestBody(t, files, "dist/sub/ok.txt"); got != "ok" {
			t.Errorf("dist/sub/ok.txt = %q", got)
		}

		// Every refused entry is reported BESIDE the files, with its reason:
		// one bad entry costs only itself.
		var skipped *SkippedEntriesError
		if !errors.As(err, &skipped) {
			t.Fatalf("err = %v (%T), want a *SkippedEntriesError beside the files", err, err)
		}
		if got, want := extractTestSkippedNames(skipped), []string{"../x", "/etc/passwd", "dist/../dist/sneaky.txt", "dist/link", "dist/null"}; !reflect.DeepEqual(got, want) {
			t.Errorf("skipped = %q, want exactly the refused entries %q", got, want)
		}
		for _, e := range skipped.Entries {
			if e.Reason == "" {
				t.Errorf("skipped %q gives no reason", e.Name)
			}
		}
		if skipped.More != 0 {
			t.Errorf("More = %d, want 0", skipped.More)
		}

		// A declared path whose only entry was refused stored nothing: it is
		// missing, the same as one that matched nothing.
		if want := []string{"../x", "/etc/passwd"}; !reflect.DeepEqual(missing, want) {
			t.Errorf("missing = %v, want %v", missing, want)
		}
	}

	declared := []string{"dist", "../x", "/etc/passwd"}

	t.Run("names and types the extractor refuses itself", func(t *testing.T) {
		files, missing, err := ExtractArtifacts(extractTestTraversalArchive(t), declared, 1<<20)
		assertRefused(t, files, missing, err)
	})

	t.Run("the tar reader's own insecure-path refusal is a skip, not a failure", func(t *testing.T) {
		// With tarinsecurepath=0 archive/tar's Next returns the header AND
		// tar.ErrInsecurePath for "../x" and "/etc/passwd". An extractor that
		// treated every Next error as fatal would drop the good files with
		// the bad ones.
		t.Setenv("GODEBUG", "tarinsecurepath=0")
		tgz := extractTestTraversalArchive(t)

		// The control: the setting reached archive/tar in this process, so
		// the reader really does answer ErrInsecurePath here. Without it this
		// subtest would pass on any extractor, because the extractor refuses
		// both names on its own.
		gz, err := gzip.NewReader(bytes.NewReader(tgz))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		if _, err := tr.Next(); err != nil { // dist/report.xml
			t.Fatal(err)
		}
		if hdr, err := tr.Next(); !errors.Is(err, tar.ErrInsecurePath) || hdr == nil || hdr.Name != "../x" {
			t.Fatalf("control: Next = (%v, %v), want the ../x header with tar.ErrInsecurePath", hdr, err)
		}

		files, missing, err := ExtractArtifacts(tgz, declared, 1<<20)
		assertRefused(t, files, missing, err)
	})

	t.Run("a backslash is a separator, so a Windows-style escape is refused too", func(t *testing.T) {
		tgz := extractTestTgz(t, []extractTestEntry{
			{name: `dist\..\..\y`, body: "escaped"},
			{name: "dist/kept.txt", body: "kept"},
		})
		files, _, err := ExtractArtifacts(tgz, []string{"dist"}, 1<<20)
		if got, want := extractTestPaths(files), []string{"dist/kept.txt"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
		var skipped *SkippedEntriesError
		if !errors.As(err, &skipped) || !reflect.DeepEqual(extractTestSkippedNames(skipped), []string{`dist\..\..\y`}) {
			t.Fatalf("err = %v, want the one escaping entry reported", err)
		}
	})

	t.Run("an entry outside every declared path is not an artifact", func(t *testing.T) {
		tgz := extractTestTgz(t, []extractTestEntry{
			{name: "dist/app.js", body: "app"},
			{name: "secrets/planted.txt", body: "not declared"},
		})
		files, missing, err := ExtractArtifacts(tgz, []string{"dist"}, 1<<20)
		if got, want := extractTestPaths(files), []string{"dist/app.js"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("files = %v, want %v", got, want)
		}
		if len(missing) != 0 {
			t.Errorf("missing = %v, want none", missing)
		}
		var skipped *SkippedEntriesError
		if !errors.As(err, &skipped) || !reflect.DeepEqual(extractTestSkippedNames(skipped), []string{"secrets/planted.txt"}) {
			t.Fatalf("err = %v, want secrets/planted.txt reported", err)
		}
	})
}

func TestExtractArtifactsReportsMissingDeclaredPaths(t *testing.T) {
	tgz := extractTestTgz(t, []extractTestEntry{
		// Metadata, not an entry to refuse: the header git archive writes.
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, pax: map[string]string{"comment": "0123abcd"}},
		{name: "dist/", typeflag: tar.TypeDir},
		{name: "dist/app.js", body: "console.log(1)"},
		{name: "dist/css/site.css", body: "body{}"},
		{name: "./report.xml", body: "<testsuite/>"},
		{name: "out/a.json", body: "{\"a\":1}"},
		{name: "out/nested/b.json", body: "{\"b\":2}"},
		{name: "build/linux/app", body: "ELF"},
		{name: "empty/", typeflag: tar.TypeDir},
	})
	declared := []string{
		"dist",         // a directory: every file under it
		"report.xml",   // a file tar stored as ./report.xml
		"coverage.out", // never produced
		"out/*.json",   // a glob the shell expanded to out/a.json
		"out/nested",   // a directory holding b.json
		"build/*",      // a glob the shell expanded to the DIRECTORY build/linux, which tar walked
		"logs/*.txt",   // a glob that matched nothing, so the shell passed it through
		"empty",        // a directory with no file in it: nothing to store
		"dist",         // declared twice: reported at most once
	}
	files, missing, err := ExtractArtifacts(tgz, declared, 1<<20)
	if err != nil {
		t.Fatalf("err = %v, want nil: nothing was refused", err)
	}
	if want := []string{"coverage.out", "logs/*.txt", "empty"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	wantPaths := []string{"build/linux/app", "dist/app.js", "dist/css/site.css", "out/a.json", "out/nested/b.json", "report.xml"}
	if got := extractTestPaths(files); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("files = %v, want %v", got, wantPaths)
	}
	if got := extractTestBody(t, files, "report.xml"); got != "<testsuite/>" {
		t.Errorf("report.xml = %q", got)
	}
	if got := extractTestBody(t, files, "out/nested/b.json"); got != "{\"b\":2}" {
		t.Errorf("out/nested/b.json = %q", got)
	}

	t.Run("nothing declared is nothing returned", func(t *testing.T) {
		files, missing, err := ExtractArtifacts(tgz, nil, 1<<20)
		if len(files) != 0 || len(missing) != 0 {
			t.Fatalf("files = %v, missing = %v, want neither", extractTestPaths(files), missing)
		}
		var skipped *SkippedEntriesError
		if !errors.As(err, &skipped) || len(skipped.Entries) != 6 {
			t.Fatalf("err = %v, want the 6 undeclared files reported", err)
		}
	})
}

func TestExtractArtifactsKeepsTheLastEntryForAPath(t *testing.T) {
	// tar appends; extracting one archive twice over the same path leaves the
	// later bytes. One path is one Library file, never two with one name.
	tgz := extractTestTgz(t, []extractTestEntry{
		{name: "dist/report.xml", body: "first"},
		{name: "dist/other.txt", body: "other"},
		{name: "./dist/report.xml", body: "second"},
	})
	files, _, err := ExtractArtifacts(tgz, []string{"dist"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := extractTestPaths(files), []string{"dist/other.txt", "dist/report.xml"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := extractTestBody(t, files, "dist/report.xml"); got != "second" {
		t.Errorf("dist/report.xml = %q, want the later entry's bytes", got)
	}
}

func TestExtractArtifactsStopsAtItsLimits(t *testing.T) {
	t.Run("total bytes beyond the limit", func(t *testing.T) {
		tgz := extractTestTgz(t, []extractTestEntry{
			{name: "dist/a.bin", body: strings.Repeat("a", 600)},
			{name: "dist/b.bin", body: strings.Repeat("b", 600)},
		})
		files, missing, err := ExtractArtifacts(tgz, []string{"dist"}, 1000)
		if !errors.Is(err, ErrArtifactsTooLarge) {
			t.Fatalf("err = %v, want ErrArtifactsTooLarge", err)
		}
		if files != nil || missing != nil {
			t.Errorf("files = %v, missing = %v, want nothing beside a refusal", extractTestPaths(files), missing)
		}
	})

	t.Run("a bomb is refused by its header before its bytes are inflated", func(t *testing.T) {
		// 16 MiB of zeros gzips to a few KiB; the header alone says it is
		// over the limit.
		tgz := extractTestTgz(t, []extractTestEntry{{name: "dist/zeros", body: strings.Repeat("\x00", 16<<20)}})
		if len(tgz) > 1<<20 {
			t.Fatalf("fixture is %d bytes; the point is a small archive that inflates", len(tgz))
		}
		if _, _, err := ExtractArtifacts(tgz, []string{"dist"}, 1<<20); !errors.Is(err, ErrArtifactsTooLarge) {
			t.Fatalf("err = %v, want ErrArtifactsTooLarge", err)
		}
	})

	t.Run("an undeclared entry's bytes count too", func(t *testing.T) {
		tgz := extractTestTgz(t, []extractTestEntry{
			{name: "elsewhere/big.bin", body: strings.Repeat("x", 2000)},
			{name: "dist/small.txt", body: "small"},
		})
		if _, _, err := ExtractArtifacts(tgz, []string{"dist"}, 1000); !errors.Is(err, ErrArtifactsTooLarge) {
			t.Fatalf("err = %v, want ErrArtifactsTooLarge", err)
		}
	})

	t.Run("more files than the Library should take from one step", func(t *testing.T) {
		entries := make([]extractTestEntry, 0, extractMaxFiles+1)
		for i := 0; i <= extractMaxFiles; i++ {
			entries = append(entries, extractTestEntry{name: fmt.Sprintf("dist/f%05d", i)})
		}
		if _, _, err := ExtractArtifacts(extractTestTgz(t, entries), []string{"dist"}, 1<<20); !errors.Is(err, ErrArtifactsTooLarge) {
			t.Fatalf("err = %v, want ErrArtifactsTooLarge", err)
		}
	})

	t.Run("not a gzip archive", func(t *testing.T) {
		files, _, err := ExtractArtifacts([]byte("sh: 1: base64: not found\n"), []string{"dist"}, 1<<20)
		if err == nil || errors.Is(err, ErrArtifactsTooLarge) {
			t.Fatalf("err = %v, want an unreadable-archive error", err)
		}
		var skipped *SkippedEntriesError
		if errors.As(err, &skipped) {
			t.Fatalf("err = %v: an unreadable archive is not a skipped entry", err)
		}
		if files != nil {
			t.Errorf("files = %v, want none", extractTestPaths(files))
		}
	})
}

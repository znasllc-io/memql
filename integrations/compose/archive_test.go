package compose

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

type zipEntry struct {
	name string
	data []byte
	link bool
}

func buildZIP(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.link {
			h.SetMode(os.ModeSymlink | 0600)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func manyEntries(n int) []zipEntry {
	out := make([]zipEntry, n)
	for i := range out {
		out[i] = zipEntry{name: fmt.Sprintf("f%03d.txt", i), data: []byte("x")}
	}
	return out
}

func TestReadArchiveCompositionLimits(t *testing.T) {
	got, err := ReadArchive(buildZIP(t, manyEntries(256)), CompositionArchiveLimits)
	if err != nil || len(got.Members) != 256 {
		t.Fatalf("256 entries: %d members, err %v", len(got.Members), err)
	}
	if _, err := ReadArchive(buildZIP(t, manyEntries(257)), CompositionArchiveLimits); err == nil || !strings.Contains(err.Error(), "256") {
		t.Fatalf("257 entries must be refused naming 256, got %v", err)
	}
}

func TestReadArchiveRefusesPathsLeavingTheArchive(t *testing.T) {
	for _, name := range []string{"../evil.txt", "/abs.txt", "a/../../b.txt", `a\b.txt`} {
		if _, err := ReadArchive(buildZIP(t, []zipEntry{{name: name, data: []byte("x")}}), CompositionArchiveLimits); err == nil {
			t.Fatalf("%q must be refused", name)
		}
	}
}

func TestReadArchiveRefusesNestedArchives(t *testing.T) {
	inner := buildZIP(t, []zipEntry{{name: "a.txt", data: []byte("x")}})
	for _, name := range []string{"inner.zip", "inner.txt"} {
		if _, err := ReadArchive(buildZIP(t, []zipEntry{{name: name, data: inner}}), CompositionArchiveLimits); err == nil || !strings.Contains(err.Error(), "nested") {
			t.Fatalf("%q: nested archive must be refused, got %v", name, err)
		}
	}
}

func TestReadArchiveRefusesLinks(t *testing.T) {
	data := buildZIP(t, []zipEntry{{name: "link.txt", data: []byte("/etc/passwd"), link: true}})
	if _, err := ReadArchive(data, CompositionArchiveLimits); err == nil {
		t.Fatal("a link must be refused")
	}
}

func TestReadArchiveSkipsUnsupportedWithReason(t *testing.T) {
	data := buildZIP(t, []zipEntry{
		{name: "logo.svg", data: []byte("<svg/>")},
		{name: "spec.pdf", data: []byte("%PDF-1.4")},
		{name: "pic.png", data: referencePNG(t)},
	})
	got, err := ReadArchive(data, CompositionArchiveLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].Path != "pic.png" || got.Members[0].MIME != "image/png" {
		t.Fatalf("members = %+v", got.Members)
	}
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped = %+v", got.Skipped)
	}
	for _, s := range got.Skipped {
		if (s.Path != "logo.svg" && s.Path != "spec.pdf") || s.Reason == "" {
			t.Fatalf("skipped = %+v", s)
		}
	}
	if _, err := ReadArchive(data, MaterializerArchiveLimits); err == nil {
		t.Fatal("the Materializer's limits keep failing the whole archive")
	}
}

func TestReadArchiveKeepsSameNamesInDifferentFolders(t *testing.T) {
	data := buildZIP(t, []zipEntry{{name: "a/email.txt", data: []byte("one")}, {name: "b/email.txt", data: []byte("two")}})
	got, err := ReadArchive(data, CompositionArchiveLimits)
	if err != nil || len(got.Members) != 2 || got.Members[0].Path != "a/email.txt" || got.Members[1].Path != "b/email.txt" {
		t.Fatalf("members = %+v, err %v", got.Members, err)
	}
}

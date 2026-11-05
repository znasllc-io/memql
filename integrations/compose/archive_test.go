package compose

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
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
		if _, err := ReadArchive(buildZIP(t, []zipEntry{{name: name, data: inner}}), MaterializerArchiveLimits); err == nil || !strings.Contains(err.Error(), "nested") {
			t.Fatalf("%q: nested archive must be refused, got %v", name, err)
		}
	}
}

// pictureHeader is the start of a PNG that says it is w by h pixels: all a
// reader looks at to know how large a picture is.
func pictureHeader(w, h uint32) []byte {
	var header bytes.Buffer
	header.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8 bits, RGB
	chunk := append([]byte("IHDR"), ihdr...)
	_ = binary.Write(&header, binary.BigEndian, uint32(len(ihdr)))
	header.Write(chunk)
	_ = binary.Write(&header, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return header.Bytes()
}

// In composition, one member that cannot be used is left out, with why, and
// the rest of the ZIP is used: a picture past the pixel limit, a file that is
// itself an archive, whatever its name, and text past the ZIP's text budget.
// The reasons are composition's own words, never the Materializer's. Its
// limits keep failing the whole archive.
func TestReadArchiveLeavesOutOneBadMemberInComposition(t *testing.T) {
	inner := buildZIP(t, []zipEntry{{name: "a.txt", data: []byte("x")}})
	limits := CompositionArchiveLimits
	limits.MaxTextBytes = 1 << 10 // the text budget, at a test's scale
	entries := []zipEntry{
		{name: "copy.txt", data: []byte("Spring.")},
		{name: "huge.png", data: pictureHeader(6000, 5000)},
		{name: "notes.txt", data: inner},
		{name: "inner.zip", data: inner},
		{name: "more.txt", data: bytes.Repeat([]byte("a"), 1100)},
		{name: "pic.png", data: referencePNG(t)},
	}
	got, err := ReadArchive(buildZIP(t, entries), limits)
	if err != nil {
		t.Fatalf("one bad member failed the whole ZIP: %v", err)
	}
	var kept []string
	for _, m := range got.Members {
		kept = append(kept, m.Path)
	}
	if strings.Join(kept, ",") != "copy.txt,pic.png" {
		t.Errorf("kept %v", kept)
	}
	want := map[string]string{
		"huge.png":  "a picture over 24 million pixels",
		"notes.txt": "an archive inside the ZIP, which is not opened",
		"inner.zip": "an archive inside the ZIP, which is not opened",
		"more.txt":  "the ZIP holds more than 1 KiB of text",
	}
	if len(got.Skipped) != len(want) {
		t.Errorf("skipped = %+v", got.Skipped)
	}
	for _, s := range got.Skipped {
		if want[s.Path] != s.Reason {
			t.Errorf("%s left out because %q, want %q", s.Path, s.Reason, want[s.Path])
		}
		if strings.Contains(s.Reason, "reference") {
			t.Errorf("%s: %q is the Materializer's wording", s.Path, s.Reason)
		}
	}

	// A ZIP that unpacks to more than the budget is still left out whole, in
	// composition's words.
	small := CompositionArchiveLimits
	small.MaxUnpackedBytes = 1 << 20
	if _, err := ReadArchive(buildZIP(t, []zipEntry{{name: "copy.txt", data: bytes.Repeat([]byte("a"), 2<<20)}}), small); err == nil || err.Error() != "it unpacks to more than 1 MiB" {
		t.Errorf("an archive past its unpacked budget: %v", err)
	}

	// The Materializer's limits fail the whole archive for each, as ever.
	for _, entry := range entries[1:4] {
		if _, err := ReadArchive(buildZIP(t, []zipEntry{entry}), MaterializerArchiveLimits); err == nil {
			t.Errorf("%s: the Materializer read the archive", entry.name)
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

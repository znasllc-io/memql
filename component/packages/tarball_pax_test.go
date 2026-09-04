package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// githubShapedTarGz packs a tree the way GitHub's tarball API does: a PAX
// global header first (git archive writes one carrying `comment=<sha>`), then
// ONE synthesized top-level directory named <owner>-<repo>-<sha> holding the
// tree. The global header is what fixtureTarGz never wrote, and it is why a
// test that passed against fixtures failed against every real repository.
func githubShapedTarGz(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeXGlobalHeader,
		Name:       "pax_global_header",
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{"comment": "bf35f7333cb739f2eb78294fbf080888d9a7dea3"},
	}); err != nil {
		t.Fatalf("write global header: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: root + "/", Mode: 0o755, Format: tar.FormatPAX}); err != nil {
		t.Fatalf("write root dir: %v", err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: root + "/" + name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX,
		}); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractTarGzStripsTheRootPastAPaxGlobalHeader pins the shape of a real
// GitHub tarball: archive/tar surfaces the PAX global header as an entry of
// its own (Typeflag 'g', Name "pax_global_header"), and the extractor must
// not count it as a second top-level name. When it did, the one synthesized
// directory was never stripped, the manifest sat one level below the returned
// root, and every repository-sourced package was refused
// package_manifest_missing while the probe -- which reads the same file
// through the contents API -- kept saying the manifest was there.
func TestExtractTarGzStripsTheRootPastAPaxGlobalHeader(t *testing.T) {
	const synthesized = "znasllc-io-memql-fylo-bf35f7333cb739f2eb78294fbf080888d9a7dea3"
	raw := githubShapedTarGz(t, synthesized, map[string]string{
		ManifestName:             "formatVersion: 1\nname: fylo\n",
		"clients/web/index.html": "<!doctype html>\n",
	})

	dest := t.TempDir()
	root, err := ExtractTarGz(bytes.NewReader(raw), dest, Limits{})
	if err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	if want := filepath.Join(dest, synthesized); root != want {
		t.Fatalf("root = %q, want the synthesized directory %q stripped as the root", root, want)
	}
	if _, serr := os.Stat(filepath.Join(root, ManifestName)); serr != nil {
		t.Fatalf("manifest is not at the returned root: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dest, "pax_global_header")); serr == nil {
		t.Fatalf("the PAX global header was materialized as a file")
	}
	if got := versionFromTarballRoot(root, `W/"unused"`); got != "bf35f7333cb739f2eb78294fbf080888d9a7dea3" {
		t.Fatalf("version = %q, want the SHA read from the stripped root's name", got)
	}
}

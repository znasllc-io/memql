package emaildoc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestEditorWriterFixture holds testdata/writer.email.json to Document.JSON:
// the file is exactly what the engine writes for the document it holds. The
// VS Code editor writes email files with a writer of its own (writeEmail in
// editors/productivity/src/emailDocument.ts), and its test reads this file and
// the built-in layouts and writes each of them again byte for byte, so the two
// writers agree on what a file can hold that JSON lets them write two ways:
// markup and characters outside ASCII written as they are, and U+2028 and
// U+2029 written as escapes, as Go's encoder always writes them. -update
// rewrites the file from the document it holds; review the diff.
func TestEditorWriterFixture(t *testing.T) {
	path := filepath.Join("testdata", "writer.email.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatalf("%s does not parse: %v", path, err)
	}
	out, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("%s is not as Document.JSON writes it (run with -update and review the diff)", path)
	}
	// What the fixture is for: each of these is written one way by Go and could
	// be written another by a JSON writer that is not Go's.
	for _, held := range []string{`\u2028`, `\u2029`, "<", "&", ">", `\"`, `\\`, `\n`, "é", "😀"} {
		if !bytes.Contains(data, []byte(held)) {
			t.Errorf("%s no longer holds %q, which the editor's writer is held to", path, held)
		}
	}
}

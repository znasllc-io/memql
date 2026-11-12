package accountexport

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE COMPATIBILITY GATE (spec D, "The promise").
//
// testdata/v<N>/ holds a file written in each format version this engine has
// produced, and every one of them must still verify on the engine as it
// stands. A newer engine reads every older file; this test is what keeps
// that true.
//
// The sample is written by the writer from the fixed fixture in
// fixture_test.go (fixed times, fixed ids), so regenerating it gives the
// same bytes:
//
//	go test github.com/znasllc-io/memql/component/accountexport/... -run TestSample -update
//
// -update writes only the CURRENT version's sample. When the format changes,
// bump FormatVersion and add testdata/v<N+1>/; never regenerate an older
// version's sample, which is the evidence of what that version wrote.
var update = flag.Bool("update", false, "write testdata/v<FormatVersion>/sample.memql-account.tar from the fixed fixture")

func samplePath(version int) string {
	return filepath.Join("testdata", fmt.Sprintf("v%d", version), "sample.memql-account.tar")
}

func TestSampleIsWrittenFromTheFixture(t *testing.T) {
	file := writeFixture(t, defaultFixture())
	path := samplePath(FormatVersion)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(file))
		return
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("FormatVersion is %d but %s does not exist: run this test with -update.\n"+
			"A format with no sample leaves the gate covering only formats nobody writes.", FormatVersion, path)
	}
}

func TestEveryCommittedSampleStillVerifies(t *testing.T) {
	samples, err := filepath.Glob(filepath.Join("testdata", "v*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 {
		t.Fatal("no samples: the compatibility promise has nothing holding it up")
	}
	for _, path := range samples {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res, err := Verify(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("a file this engine once wrote no longer verifies: %v\n"+
					"Fix the READER, never the sample.", err)
			}
			dir := filepath.Base(filepath.Dir(path))
			if want := fmt.Sprintf("v%d", res.Manifest.FormatVersion); dir != want {
				t.Errorf("%s is under %s but says format %d", path, dir, res.Manifest.FormatVersion)
			}
			if len(res.Report.Kinds) == 0 {
				t.Error("the sample counts no kinds, so it proves nothing about rows")
			}
		})
	}
}

// The sample keeps a non-ASCII payload, the case a careless encoding change
// breaks first.
func TestSampleKeepsNonASCII(t *testing.T) {
	data, err := os.ReadFile(samplePath(1))
	if err != nil {
		t.Fatal(err)
	}
	es := readEntries(t, data)
	found := false
	for _, p := range partsOf(t, data, "rows/library/file") {
		found = found || strings.Contains(strings.Join(p, "\n"), "Café")
	}
	if !found || len(es) == 0 {
		t.Error("the v1 sample lost its non-ASCII payload")
	}
}

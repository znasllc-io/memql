package backup

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE COMPATIBILITY GATE.
//
// testdata/ holds a real backup written in each format version this engine has
// ever produced. Every one of them must still be readable, in full, by the
// engine as it stands today. That is the promise the package doc makes, and
// this is the only thing that keeps it: a compatibility rule with no gate
// decays within two releases -- memql#3600 is the local proof, where the rule
// ("an explicit env entry beats envFrom") existed in a comment, nothing checked
// it, and five variables quietly stopped obeying it.
//
// WHEN THE FORMAT CHANGES: bump FormatVersion, and ADD a fixture here. Never
// edit an existing one. An old fixture is evidence about what shipped; editing
// it to make a test pass is forging the evidence, and the next person to
// restore a real backup from that release finds out.
//
// A fixture is deliberately hand-written JSON rather than generated: generating
// it with the current writer would make the test tautological -- it would prove
// only that today's writer agrees with today's reader, which is not the
// question.
func TestEveryCommittedFixtureStillRestores(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "format-v*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures: the compatibility promise has nothing holding it up")
	}

	for _, path := range fixtures {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			r, err := NewReader(f)
			if err != nil {
				t.Fatalf("a backup this engine once wrote is no longer readable: %v\n"+
					"That breaks the promise in the package doc. Fix the READER, "+
					"never the fixture.", err)
			}

			m := r.Manifest()
			if m.FormatVersion > FormatVersion {
				t.Fatalf("fixture claims format %d but this engine reads %d", m.FormatVersion, FormatVersion)
			}

			seen := map[string]int{}
			for {
				row, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("row unreadable in a shipped format: %v", err)
				}
				if row.ID == "" || row.Concept == "" {
					t.Errorf("row lost its identity on read: %+v", row)
				}
				// Payload must survive as parseable JSON -- a restore writes it
				// straight into a JSONB column.
				if len(row.Payload) == 0 {
					t.Errorf("row %s has an empty payload", row.ID)
				}
				seen[row.Table]++
			}

			// The manifest's counts are a claim about the file. If they and the
			// rows disagree, one of them is lying and a restore would silently
			// do the wrong amount of work.
			for table, want := range m.Counts {
				if seen[table] != want {
					t.Errorf("manifest claims %d rows in %s, stream carries %d",
						want, table, seen[table])
				}
			}
		})
	}
}

// A fixture must exist for the CURRENT format version, or the gate silently
// stops covering what the engine actually writes today.
func TestCurrentFormatVersionHasAFixture(t *testing.T) {
	want := filepath.Join("testdata", "format-v"+itoa(FormatVersion)+".jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("FormatVersion is %d but %s does not exist.\n"+
			"Bumping the format without adding a fixture leaves the compatibility "+
			"gate covering only formats nobody writes any more.", FormatVersion, want)
	}
}

// The fixture must be the hand-written article, not something a generator
// produced from the current writer -- otherwise it proves only that today's
// code agrees with itself.
func TestFixturesAreReadableAsPlainText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "format-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), `{"kind":"memql.backup.manifest"`) {
		t.Error("the fixture does not begin with a manifest line")
	}
	if !strings.Contains(string(body), "Café") {
		t.Error("the fixture lost its non-ASCII payload, which is the case most " +
			"likely to break under a careless encoding change")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

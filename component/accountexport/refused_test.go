package accountexport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A reference a row makes to bytes outside the place its kind allows is
// recorded as refused and never read (design section B, "Bytes").

var refusedMeta = BytesMeta{
	Key:    "library/someone-else/f9/report.pdf",
	UsedBy: []RowField{{Concept: "v1:library:file", ID: "v1:library:file:f9", Field: "blobUrl"}},
}

// writeRefusedFile writes the account's rows and one refused reference, with
// a report counting `reported` refused references.
func writeRefusedFile(t *testing.T, reported int64) ([]byte, error) {
	t.Helper()
	c := defaultFixture()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range c.accountRows {
		if err := w.AccountRow(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.RefusedBytes(refusedMeta); err != nil {
		t.Fatalf("RefusedBytes: %v", err)
	}
	r := c.report()
	r.Members = nil
	r.Kinds = r.Kinds[:1]
	r.Bytes = BytesReport{Refused: reported}
	err = w.Finish(r)
	return buf.Bytes(), err
}

func TestRefusedReferenceRoundTrips(t *testing.T) {
	file, err := writeRefusedFile(t, 1)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	res, err := Verify(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Report.Bytes.Refused != 1 {
		t.Errorf("report counts %d refused, want 1", res.Report.Bytes.Refused)
	}
	for _, e := range readEntries(t, file) {
		switch {
		case strings.HasSuffix(e.name, "/"+MemberBytesIndex):
			var line BytesLine
			if err := json.Unmarshal(bytes.TrimSpace(e.data), &line); err != nil {
				t.Fatal(err)
			}
			if !line.Refused || line.Missing || line.File != "" || line.Size != 0 || line.SHA256 != "" {
				t.Errorf("refused line = %+v, want refused with no file, size or checksum", line)
			}
			if line.Key != refusedMeta.Key || len(line.UsedBy) != 1 || line.UsedBy[0].Field != "blobUrl" {
				t.Errorf("refused line lost the key or the row: %+v", line)
			}
		case strings.HasPrefix(e.name, fixtureStem+"/bytes/"):
			t.Errorf("a refused reference wrote a member: %s", e.name)
		case strings.HasSuffix(e.name, "/"+MemberReadme):
			if !strings.Contains(string(e.data), "Refused and not read") {
				t.Errorf("README does not say a reference was refused:\n%s", e.data)
			}
		}
	}
}

func TestFinishRefusesAReportThatMiscountsRefused(t *testing.T) {
	_, err := writeRefusedFile(t, 0)
	mustContain(t, err, "refused")
}

func TestRefusedIsNeverAlsoMissing(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	both := refusedMeta
	both.Missing = true
	mustContain(t, w.RefusedBytes(both), "never both")

	w2, err := NewWriter(&bytes.Buffer{}, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	both = refusedMeta
	both.Refused = true
	mustContain(t, w2.MissingBytes(both), "never both")

	w3, err := NewWriter(&bytes.Buffer{}, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, w3.Bytes(both, 1, strings.NewReader("x")), "refused")
}

// editBytesIndex rewrites bytes.jsonl and report.json of a refused file and
// re-sums it, so only what Verify reads inside the members can catch it.
func editRefusedFile(t *testing.T, line func(*BytesLine), report func(*Report)) []byte {
	t.Helper()
	file, err := writeRefusedFile(t, 1)
	if err != nil {
		t.Fatal(err)
	}
	es := readEntries(t, file)
	for i := range es {
		switch {
		case strings.HasSuffix(es[i].name, "/"+MemberBytesIndex) && line != nil:
			var l BytesLine
			if err := json.Unmarshal(bytes.TrimSpace(es[i].data), &l); err != nil {
				t.Fatal(err)
			}
			line(&l)
			b, _ := json.Marshal(l)
			es[i].data = append(b, '\n')
		case strings.HasSuffix(es[i].name, "/"+MemberReport) && report != nil:
			var r Report
			if err := json.Unmarshal(es[i].data, &r); err != nil {
				t.Fatal(err)
			}
			report(&r)
			es[i].data, _ = json.Marshal(r)
		}
	}
	return writeEntries(t, resum(es))
}

func TestVerifyRefusesARefusedCountThatDiffersFromTheLines(t *testing.T) {
	_, err := Verify(bytes.NewReader(editRefusedFile(t, nil, func(r *Report) { r.Bytes.Refused = 2 })))
	mustContain(t, err, "refused")
}

func TestVerifyRefusesARefusedLineNamingAFile(t *testing.T) {
	for name, edit := range map[string]func(*BytesLine){
		"file":     func(l *BytesLine) { l.File = "bytes/000001" },
		"size":     func(l *BytesLine) { l.Size = 10 },
		"checksum": func(l *BytesLine) { l.SHA256 = strings.Repeat("0", 64) },
		"missing":  func(l *BytesLine) { l.Missing = true },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Verify(bytes.NewReader(editRefusedFile(t, edit, nil)))
			mustContain(t, err, refusedMeta.Key)
		})
	}
}

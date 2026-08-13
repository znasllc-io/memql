package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func fixedTime() time.Time { return time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC) }

// sampleRows are deliberately awkward: a unicode payload, an empty metadata
// object, a nested structure, and a secret-table row. A format is only worth
// anything if it survives the data people actually have.
func sampleRows() []Row {
	return []Row{
		{
			Table: TableMemoryNodes, ID: "v1:cognition:space:abc", CreatedAt: fixedTime(),
			CreatedBy: "system:identity-svc", Concept: "v1:cognition:space", Type: "space",
			Schema:     json.RawMessage(`{"v":1}`),
			Payload:    json.RawMessage(`{"name":"Café — naïve ☕","nested":{"a":[1,2,3]}}`),
			Metadata:   json.RawMessage(`{}`),
			Provenance: json.RawMessage(`{"mutation":"mutationCreateSpace"}`),
		},
		{
			Table: TableSecretMemoryNodes, ID: "v1:identity:identity:pat-1", CreatedAt: fixedTime().Add(time.Second),
			CreatedBy: "system:identity-svc", Concept: "v1:identity:identity", Type: "identity",
			Schema:     json.RawMessage(`{"v":1}`),
			Payload:    json.RawMessage(`{"cipher":"AAAA"}`),
			Provenance: json.RawMessage(`{"mutation":"createPat"}`),
		},
	}
}

func writeStream(t *testing.T, rows []Row, m Manifest) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatal(err)
	}
	w := newWriter(&buf)
	for _, r := range rows {
		if err := w.WriteRow(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A backup that changes the data it carries is not a backup. Payloads travel as
// RAW JSON precisely so an engine cannot silently normalise something it does
// not understand.
func TestRoundTripPreservesRowsByteForByte(t *testing.T) {
	rows := sampleRows()
	m := ManifestFor("0.17.0", "memql.localhost", KeyFingerprint("k"), true, map[string]int{}, fixedTime())

	r, err := NewReader(bytes.NewReader(writeStream(t, rows, m)))
	if err != nil {
		t.Fatal(err)
	}

	var got []Row
	for {
		row, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if len(got) != len(rows) {
		t.Fatalf("read %d rows, want %d", len(got), len(rows))
	}
	for i := range rows {
		if got[i].ID != rows[i].ID || got[i].Concept != rows[i].Concept || got[i].Table != rows[i].Table {
			t.Errorf("row %d identity changed: %+v", i, got[i])
		}
		if string(got[i].Payload) != string(rows[i].Payload) {
			t.Errorf("row %d payload was rewritten:\n got %s\nwant %s", i, got[i].Payload, rows[i].Payload)
		}
		if !got[i].CreatedAt.Equal(rows[i].CreatedAt) {
			t.Errorf("row %d createdAt changed: %v vs %v", i, got[i].CreatedAt, rows[i].CreatedAt)
		}
		if string(got[i].Provenance) != string(rows[i].Provenance) {
			t.Errorf("row %d lost its provenance", i)
		}
	}
}

// THE COMPATIBILITY PROMISE, in the direction it is made: a newer engine reads
// every older backup. A format-1 stream must stay readable forever.
func TestOlderFormatIsAccepted(t *testing.T) {
	stream := writeStream(t, sampleRows(),
		ManifestFor("0.17.0", "memql.localhost", "", true, map[string]int{}, fixedTime()))

	if _, err := NewReader(bytes.NewReader(stream)); err != nil {
		t.Fatalf("a format-%d backup must remain readable: %v", FormatVersion, err)
	}
}

// And the direction it is NOT made: a newer stream is refused outright, with an
// error an operator can act on. Importing part of a stream this binary does not
// fully understand would look like success, which is worse than stopping.
func TestNewerFormatIsRefusedNotPartiallyRead(t *testing.T) {
	stream := writeStream(t, sampleRows(),
		ManifestFor("99.0.0", "memql.localhost", "", true, map[string]int{}, fixedTime()))
	// Forge a later format version in the header.
	stream = bytes.Replace(stream,
		[]byte(`"formatVersion":1`), []byte(`"formatVersion":999`), 1)

	_, err := NewReader(bytes.NewReader(stream))
	if err == nil {
		t.Fatal("a newer format was accepted; a half-restore looks like success")
	}
	var tooNew *ErrFormatTooNew
	if !errors.As(err, &tooNew) {
		t.Fatalf("want ErrFormatTooNew, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "at least as new") {
		t.Errorf("the refusal does not tell the operator what to do: %v", err)
	}
}

// Forward compatibility, the cheap half: an unknown record kind is SKIPPED, so
// a later format can add a trailer without breaking this reader.
func TestUnknownRecordKindsAreSkipped(t *testing.T) {
	stream := writeStream(t, sampleRows(),
		ManifestFor("0.17.0", "", "", true, map[string]int{}, fixedTime()))
	stream = append(stream, []byte(`{"kind":"memql.backup.checksum","sha256":"deadbeef"}`+"\n")...)

	r, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("an unknown kind should be skipped, not fatal: %v", err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("read %d rows, want 2", n)
	}
}

// A truncated or foreign file must fail on the FIRST line, before anything is
// restored.
func TestGarbageIsRefusedBeforeAnyRow(t *testing.T) {
	for name, body := range map[string]string{
		"empty":       "",
		"not json":    "hello\n",
		"wrong kind":  `{"kind":"something.else","formatVersion":1}` + "\n",
		"no version":  `{"kind":"memql.backup.manifest"}` + "\n",
		"pg_dump-ish": "--\n-- PostgreSQL database dump\n--\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReader(strings.NewReader(body)); err == nil {
				t.Fatal("accepted a file that is not a memQL backup")
			}
		})
	}
}

// The fingerprint identifies the key without disclosing it, and differs per key
// -- which is what lets restore tell an operator their secret rows will not be
// readable here.
func TestKeyFingerprint(t *testing.T) {
	a := KeyFingerprint("master-key-one")
	b := KeyFingerprint("master-key-two")

	if a == "" || b == "" {
		t.Fatal("a present key must produce a fingerprint")
	}
	if a == b {
		t.Error("different keys produced the same fingerprint")
	}
	if a != KeyFingerprint("master-key-one") {
		t.Error("fingerprint is not stable for the same key")
	}
	if strings.Contains(a, "master-key-one") {
		t.Error("the fingerprint leaks the key")
	}
	if KeyFingerprint("") != "" {
		t.Error("no key must produce no fingerprint, not a fingerprint of the empty string")
	}
}

// Counts are written into the header, so a reader knows what a complete file
// should contain before it starts.
func TestManifestCarriesCounts(t *testing.T) {
	var buf bytes.Buffer
	w := newWriter(&buf)
	for _, r := range sampleRows() {
		if err := w.WriteRow(r); err != nil {
			t.Fatal(err)
		}
	}
	counts := w.Counts()

	if counts[TableMemoryNodes] != 1 || counts[TableSecretMemoryNodes] != 1 {
		t.Fatalf("counts = %v, want one per table", counts)
	}
	m := ManifestFor("0.17.0", "memql.localhost", "", true, counts, fixedTime())
	if m.Counts[TableMemoryNodes] != 1 {
		t.Errorf("manifest lost the counts: %+v", m)
	}
	if m.FormatVersion != FormatVersion {
		t.Errorf("manifest formatVersion = %d, want %d", m.FormatVersion, FormatVersion)
	}
}

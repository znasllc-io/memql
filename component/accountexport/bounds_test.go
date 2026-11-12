package accountexport

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// withLimits shrinks the bounds for one test, so a bound can be reached
// without writing megabytes, and puts them back after.
func withLimits(t *testing.T, edit func(l *limits)) {
	t.Helper()
	saved := bounds
	edit(&bounds)
	t.Cleanup(func() { bounds = saved })
}

// rowOfLine answers a row whose encoded line is exactly n bytes.
func rowOfLine(t *testing.T, concept, id string, n int) Row {
	t.Helper()
	base := fixtureRow(concept, id, 1, `{"f":""}`)
	line, err := encodeLine(Row{Kind: KindRow, Concept: base.Concept, ID: base.ID, CreatedAt: base.CreatedAt,
		CreatedBy: base.CreatedBy, Type: base.Type, Schema: base.Schema, Payload: base.Payload, Provenance: base.Provenance})
	if err != nil {
		t.Fatal(err)
	}
	pad := n - len(line)
	if pad < 0 {
		t.Fatalf("a row line is at least %d bytes, cannot make one of %d", len(line), n)
	}
	return fixtureRow(concept, id, 1, `{"f":"`+strings.Repeat("x", pad)+`"}`)
}

// peopleLineOf answers a people line whose encoding is exactly n bytes.
func peopleLineOf(t *testing.T, n int) map[string]any {
	t.Helper()
	empty, _ := encodeLine(map[string]any{"id": ""})
	return map[string]any{"id": strings.Repeat("u", n-len(empty))}
}

// finishAndVerify finishes a file with the report the caller passes and
// verifies it.
func finishAndVerify(t *testing.T, buf *bytes.Buffer, w *Writer, r Report) {
	t.Helper()
	if err := w.Finish(r); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if _, err := Verify(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Verify refused a file the writer finished: %v", err)
	}
}

func newFixtureWriter(t *testing.T) (*bytes.Buffer, *Writer) {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	return &buf, w
}

func accountKinds(rows, versions int64) []KindCount {
	return []KindCount{{Concept: "v1:accounts:account", ExpectedRows: rows, ExpectedVersions: versions, WrittenRows: rows, WrittenVersions: versions}}
}

// The writer holds every bound the checker holds, so a file it finishes
// always passes Verify: at each bound the write succeeds and the file
// verifies; one byte past it the write fails naming the member and the bound.
func TestWriterHoldsEveryBound(t *testing.T) {
	t.Run("line", func(t *testing.T) {
		withLimits(t, func(l *limits) { l.lineBytes = 400 })
		buf, w := newFixtureWriter(t)
		if err := w.AccountRow(rowOfLine(t, "v1:accounts:account", "a", 400)); err != nil {
			t.Fatalf("a line at the bound: %v", err)
		}
		p := w.Parted(RowsDir("v1:todos:todo"))
		if err := p.Write(rowOfLine(t, "v1:todos:todo", "t", 400)); err != nil {
			t.Fatalf("a part line at the bound: %v", err)
		}
		mustContain(t, p.Write(rowOfLine(t, "v1:todos:todo", "u", 401)), "rows/todos/todo", "400")

		_, w2 := newFixtureWriter(t)
		mustContain(t, w2.AccountRow(rowOfLine(t, "v1:accounts:account", "a", 401)), MemberAccount, "400")
		_, w3 := newFixtureWriter(t)
		mustContain(t, w3.Line(MemberPeople, peopleLineOf(t, 401)), MemberPeople, "400")

		// The at-bound file verifies.
		buf, w = newFixtureWriter(t)
		if err := w.AccountRow(rowOfLine(t, "v1:accounts:account", "a", 400)); err != nil {
			t.Fatal(err)
		}
		if err := w.Line(MemberPeople, peopleLineOf(t, 400)); err != nil {
			t.Fatal(err)
		}
		finishAndVerify(t, buf, w, Report{Kinds: accountKinds(1, 1), Members: []MemberCount{{Name: MemberPeople, Lines: 1}}})
	})

	t.Run("part lines, compressed and decompressed bytes", func(t *testing.T) {
		withLimits(t, func(l *limits) {
			l.partLines = 3
			l.partBytes = 600
			l.partRawBytes = 2000
			l.lineBytes = 1400
			l.partStoredBytes = 1400 + 512
		})
		buf, w := newFixtureWriter(t)
		p := w.Parted(RowsDir("v1:todos:todo"))
		var rows []Row
		for i := 0; i < 20; i++ {
			// Compressible lines exercise the decompressed bound, an
			// incompressible one the compressed bound, short ones the count.
			n := 300 + (i%4)*300
			r := rowOfLine(t, "v1:todos:todo", fmt.Sprintf("t%02d", i), n)
			if i == 7 {
				r.Payload = json.RawMessage(`{"f":"` + noise(1000) + `"}`)
			}
			rows = append(rows, r)
			if err := p.Write(r); err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		finishAndVerify(t, buf, w, Report{Kinds: append(accountKinds(0, 0), KindCount{Concept: "v1:todos:todo", ExpectedRows: 20, ExpectedVersions: 20, WrittenRows: 20, WrittenVersions: 20})})
		for _, e := range readEntries(t, buf.Bytes()) {
			if !strings.HasSuffix(e.name, ".jsonl.gz") {
				continue
			}
			zr, _ := gzip.NewReader(bytes.NewReader(e.data))
			raw, _ := io.ReadAll(zr)
			lines := bytes.Count(raw, []byte("\n"))
			if lines > 3 || len(raw) > 2000 || len(e.data) > 1400+512 || (lines > 1 && len(e.data) > 600) {
				t.Errorf("%s: %d lines, %d bytes raw, %d stored: past a bound", e.name, lines, len(raw), len(e.data))
			}
		}
	})

	t.Run("schema", func(t *testing.T) {
		withLimits(t, func(l *limits) { l.schemaBytes = 64 })
		buf, w := newFixtureWriter(t)
		at := json.RawMessage(`{"d":"` + strings.Repeat("s", 64-8) + `"}`)
		if err := w.Schema("a", at); err != nil {
			t.Fatalf("a schema at the bound: %v", err)
		}
		finishAndVerify(t, buf, w, Report{Kinds: accountKinds(0, 0)})
		_, w2 := newFixtureWriter(t)
		over := json.RawMessage(`{"d":"` + strings.Repeat("s", 64-7) + `"}`)
		mustContain(t, w2.Schema("a", over), "schemas/a.json", "64")
	})

	t.Run("single member", func(t *testing.T) {
		withLimits(t, func(l *limits) { l.singleBytes = 300 })
		_, w := newFixtureWriter(t)
		if err := w.Line(MemberPeople, peopleLineOf(t, 150)); err != nil {
			t.Fatal(err)
		}
		if err := w.Line(MemberPeople, peopleLineOf(t, 150)); err != nil {
			t.Fatalf("a member at the bound: %v", err)
		}
		mustContain(t, w.Line(MemberPeople, peopleLineOf(t, 20)), MemberPeople, "300")
	})

	t.Run("bytes.jsonl", func(t *testing.T) {
		withLimits(t, func(l *limits) { l.singleBytes = 300 })
		_, w := newFixtureWriter(t)
		var err error
		for i := 0; i < 20 && err == nil; i++ {
			err = w.MissingBytes(BytesMeta{Key: fmt.Sprintf("files/%02d", i)})
		}
		mustContain(t, err, MemberBytesIndex, "300")

		_, w = newFixtureWriter(t)
		err = nil
		for i := 0; i < 20 && err == nil; i++ {
			err = w.Bytes(BytesMeta{Key: fmt.Sprintf("files/%02d", i)}, 1, strings.NewReader("x"))
		}
		mustContain(t, err, MemberBytesIndex, "300")
	})

	t.Run("report and README", func(t *testing.T) {
		_, w := newFixtureWriter(t)
		r := Report{Kinds: accountKinds(0, 0), Rebuilt: []string{strings.Repeat("r", 400)}}
		withLimits(t, func(l *limits) { l.singleBytes = 300 })
		mustContain(t, w.Finish(r), MemberReport, "300")
	})

	t.Run("SHA256SUMS", func(t *testing.T) {
		// Room for the manifest's line and a few more, not for the whole file.
		withLimits(t, func(l *limits) { l.sumsBytes = 300 })
		_, w := newFixtureWriter(t)
		mustContain(t, w.Finish(Report{Kinds: accountKinds(0, 0)}), MemberSums, "300")
	})

	t.Run("manifest", func(t *testing.T) {
		withLimits(t, func(l *limits) { l.manifestBytes = 100 })
		_, err := NewWriter(&bytes.Buffer{}, fixtureStem, fixtureManifest())
		mustContain(t, err, MemberManifest, "100")
	})
}

func noise(n int) string {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte('a' + x%26)
	}
	return string(b)
}

// Verify refuses a member one byte (or one line) over each bound, naming the
// member: each bound is set to the size the fixture's member has, which
// verifies, then to one less, which must not.
func TestVerifyRefusesAMemberOverEachBound(t *testing.T) {
	file := writeFixture(t, defaultFixture())
	es := readEntries(t, file)
	size := func(rel string) int {
		for _, e := range es {
			if e.name == fixtureStem+"/"+rel {
				return len(e.data)
			}
		}
		t.Fatalf("no member %s", rel)
		return 0
	}
	part := "rows/library/file/part-000001.jsonl.gz"
	zr, err := gzip.NewReader(bytes.NewReader(es[2].data))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	longest := 0
	for _, l := range bytes.SplitAfter(raw, []byte("\n")) {
		longest = max(longest, len(l))
	}
	peopleLongest := 0
	for _, l := range bytes.SplitAfter(es[5].data, []byte("\n")) {
		peopleLongest = max(peopleLongest, len(l))
	}
	// The line bound is held over the whole file, so it is set from its
	// longest line wherever that is.
	fileLongest := max(longest, peopleLongest)
	for _, e := range es {
		if strings.HasSuffix(e.name, ".jsonl") {
			for _, l := range bytes.SplitAfter(e.data, []byte("\n")) {
				fileLongest = max(fileLongest, len(l))
			}
		}
	}

	// The single-member bound is set from the largest single member.
	biggest := ""
	for _, rel := range []string{MemberAccount, MemberPeople, MemberReferences, MemberConnections, MemberBytesIndex, MemberReport, MemberReadme} {
		if biggest == "" || size(rel) > size(biggest) {
			biggest = rel
		}
	}

	cases := []struct {
		name   string
		member string
		at     int
		set    func(l *limits, n int)
	}{
		{"manifest", MemberManifest, size(MemberManifest), func(l *limits, n int) { l.manifestBytes = n }},
		{"schema", "schemas/" + fixtureSchema + ".json", size("schemas/" + fixtureSchema + ".json"), func(l *limits, n int) { l.schemaBytes = n }},
		{"SHA256SUMS", MemberSums, size(MemberSums), func(l *limits, n int) { l.sumsBytes = n }},
		{"single member", biggest, size(biggest), func(l *limits, n int) { l.singleBytes = n }},
		{"part stored bytes", part, size(part), func(l *limits, n int) { l.partStoredBytes = n }},
		{"part decompressed bytes", part, len(raw), func(l *limits, n int) { l.partRawBytes = n }},
		{"part lines", part, bytes.Count(raw, []byte("\n")), func(l *limits, n int) { l.partLines = n }},
		{"line", "", fileLongest, func(l *limits, n int) { l.lineBytes = n }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withLimits(t, func(l *limits) { c.set(l, c.at) })
			if _, err := Verify(bytes.NewReader(file)); err != nil {
				t.Fatalf("at the bound (%d): %v", c.at, err)
			}
			c.set(&bounds, c.at-1)
			_, err := Verify(bytes.NewReader(file))
			want := []string{fmt.Sprint(c.at - 1)}
			if c.member != "" {
				want = append(want, c.member)
			}
			mustContain(t, err, want...)
		})
	}
}

// "Too new" is decided before anything else in the manifest is looked at, so
// a future manifest whose other fields changed shape is still reported as
// too new and not as malformed.
func TestTooNewIsDecidedBeforeTheRestOfTheManifest(t *testing.T) {
	m := `{"kind":7,"formatVersion":2,"createdAt":12,"kinds":"all of them","account":[1]}`
	file := writeEntries(t, []entry{{fixtureStem + "/" + MemberManifest, []byte(m)}})
	_, err := Verify(bytes.NewReader(file))
	var tooNew *ErrFormatTooNew
	if !errors.As(err, &tooNew) || tooNew.Found != 2 {
		t.Fatalf("want ErrFormatTooNew, got %v", err)
	}
}

// Verify keeps a running total of the checksum list it is building as members
// are read, and refuses once it passes the writer's bound, before it reaches
// SHA256SUMS: a file of empty bytes/ objects is refused at the bound, not after
// every one of them is held. The writer refuses such a file before writing it,
// so the input is built by hand.
func TestVerifyBoundsTheChecksumListAsItReads(t *testing.T) {
	file := writeFixture(t, defaultFixture())
	manifest := readEntries(t, file)[0]
	es := []entry{manifest}
	for n := 1; n <= 2000; n++ {
		es = append(es, entry{fixtureStem + "/" + objectFile(n), nil})
	}
	withLimits(t, func(l *limits) { l.sumsBytes = 10_000 })
	_, err := Verify(bytes.NewReader(writeEntries(t, es)))
	mustContain(t, err, MemberSums, "10000", "bytes in the checksum list")

	// The fixture's own list, at the bound, verifies; one byte under, it is
	// refused while the members are read, naming the bound.
	sums := 0
	for _, e := range readEntries(t, file) {
		if !strings.HasSuffix(e.name, "/"+MemberSums) {
			sums += sumLineBytes(strings.TrimPrefix(e.name, fixtureStem+"/"))
		}
	}
	withLimits(t, func(l *limits) { l.sumsBytes = sums })
	if _, err := Verify(bytes.NewReader(file)); err != nil {
		t.Fatalf("at the bound (%d): %v", sums, err)
	}
	// Without its SHA256SUMS, so the refusal can only come from the running
	// total: the file is otherwise cut short.
	bounds.sumsBytes = sums - 1
	noSums := []entry{}
	for _, e := range readEntries(t, file) {
		if !strings.HasSuffix(e.name, "/"+MemberSums) {
			noSums = append(noSums, e)
		}
	}
	_, err = Verify(bytes.NewReader(writeEntries(t, noSums)))
	mustContain(t, err, MemberSums, fmt.Sprint(sums-1), "bytes in the checksum list")
}

// After the archive's end a file may carry only zero blocks: a tar writer may
// pad the archive to its record size with them. Anything else is refused.
func TestVerifyRefusesAnythingButZeroBlocksAfterTheEnd(t *testing.T) {
	file := writeFixture(t, defaultFixture())
	padded := append(append([]byte{}, file...), make([]byte, 20*512)...)
	if _, err := Verify(bytes.NewReader(padded)); err != nil {
		t.Fatalf("zero blocks after the end: %v", err)
	}
	for name, tail := range map[string][]byte{
		"a block that is not zero": append(make([]byte, 511), 'x'),
		"text":                     []byte("appended\n"),
		"zeros that are no block":  make([]byte, 100),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Verify(bytes.NewReader(append(append([]byte{}, file...), tail...)))
			mustContain(t, err, "after the archive's end")
		})
	}
}

// account.jsonl holds the account's own row and nothing else: a row of another
// declared kind there is refused. The writer is not handed such a row, so the
// member is rewritten by hand and the checksum list made to agree.
func TestVerifyRefusesARowOfAnotherKindInTheAccountMember(t *testing.T) {
	es := readEntries(t, writeFixture(t, defaultFixture()))
	misfiled := fixtureRow("v1:todos:todo", "t9", 3, `{"title":"Misfiled"}`)
	misfiled.Kind = KindRow
	line, err := encodeLine(misfiled)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, e := range es {
		if e.name == fixtureStem+"/"+MemberAccount {
			es[i].data = append(append([]byte{}, e.data...), line...)
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s", MemberAccount)
	}
	_, err = Verify(bytes.NewReader(writeEntries(t, resum(es))))
	mustContain(t, err, MemberAccount, "v1:todos:todo")
}

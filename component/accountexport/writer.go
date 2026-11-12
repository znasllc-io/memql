package accountexport

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// memberTime is the modification time of every member. Fixed, so that two
// files of an unchanged account differ only in the manifest, the report and
// the README (spec D, "Deterministic"); the snapshot's instant is in the
// manifest.
var memberTime = time.Unix(0, 0).UTC()

// Writer writes one file. Members go out in the layout's order: a call that
// would put a member before one already written is refused. Every refusal is
// final: once a call fails the stream is not a file, and every later call
// answers the same error, so a caller cannot finish a file that lost a
// member.
type Writer struct {
	tw    *tar.Writer
	stem  string
	m     Manifest
	err   error
	phase int
	open  *PartWriter

	declared map[string]bool
	lines    map[string]*bytes.Buffer // single line members not yet written
	dirs     map[string]bool          // parted folders already opened
	schemas  map[string]bool
	sums     []sumLine
	counts   *tally
	acctLast string
	sumsLen  int

	bytesIndex bytes.Buffer
	objects    int
}

// NewWriter writes manifest.json first. stem names the folder inside the
// tar. Kind and FormatVersion are stamped here; the caller does not set them.
func NewWriter(w io.Writer, stem string, m Manifest) (*Writer, error) {
	if !safeSegment(stem) {
		return nil, fmt.Errorf("accountexport: %q cannot name the folder", stem)
	}
	if m.Account.ID == "" {
		return nil, errors.New("accountexport: the manifest names no account")
	}
	m.Kind = KindManifest
	m.FormatVersion = FormatVersion
	if m.Kinds == nil {
		m.Kinds = []KindDecl{}
	}
	declared := map[string]bool{}
	for _, k := range m.Kinds {
		if RowsDir(k.Concept) == "" {
			return nil, fmt.Errorf("accountexport: the manifest declares %q, which is not a concept of the form v1:<namespace>:<name>", k.Concept)
		}
		if declared[k.Concept] {
			return nil, fmt.Errorf("accountexport: the manifest declares %s twice", k.Concept)
		}
		declared[k.Concept] = true
	}
	wr := &Writer{
		tw:       tar.NewWriter(w),
		stem:     stem,
		m:        m,
		declared: declared,
		lines:    map[string]*bytes.Buffer{},
		dirs:     map[string]bool{},
		schemas:  map[string]bool{},
		counts:   newTally(),
	}
	doc, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("accountexport: manifest: %w", err)
	}
	if err := wr.member(MemberManifest, append(doc, '\n')); err != nil {
		return nil, err
	}
	return wr, nil
}

// fail records the first error; every later call answers it.
func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = fmt.Errorf("accountexport: %w", err)
	}
	return w.err
}

// member writes one whole member and records its digest. It refuses a
// member past the bound Verify will hold it to, before writing a byte of it.
func (w *Writer) member(rel string, data []byte) error {
	if bound, what := memberBound(rel); len(data) > bound {
		return w.fail(fmt.Errorf("%s: %w", rel, over(int64(len(data)), what, bound)))
	}
	if err := w.reserveSum(rel); err != nil {
		return err
	}
	if err := w.tw.WriteHeader(w.header(rel, int64(len(data)))); err != nil {
		return w.fail(fmt.Errorf("%s: %w", rel, err))
	}
	if _, err := w.tw.Write(data); err != nil {
		return w.fail(fmt.Errorf("%s: %w", rel, err))
	}
	d := sha256.Sum256(data)
	w.sums = append(w.sums, sumLine{path: rel, digest: hex.EncodeToString(d[:])})
	return nil
}

// reserveSum makes room for rel's line in SHA256SUMS, refusing the member
// that would take the list past its bound: the list is written last, and a
// writer that found out only then would have written the whole file for
// nothing.
func (w *Writer) reserveSum(rel string) error {
	if len(rel) > relMaxBytes {
		return w.fail(fmt.Errorf("%s: %w", rel, over(int64(len(rel)), "bytes in a path", relMaxBytes)))
	}
	n := w.sumsLen + sumLineBytes(rel)
	if n > bounds.sumsBytes {
		return w.fail(fmt.Errorf("%s: adding %s: %w", MemberSums, rel, over(int64(n), "bytes in the checksum list", bounds.sumsBytes)))
	}
	w.sumsLen = n
	return nil
}

// line encodes one JSON line of member and holds it to the line bound.
func (w *Writer) line(member string, v any) ([]byte, error) {
	line, err := encodeLine(v)
	if err != nil {
		return nil, w.fail(fmt.Errorf("%s: %w", member, err))
	}
	if len(line) > bounds.lineBytes {
		return nil, w.fail(fmt.Errorf("%s: %w", member, over(int64(len(line)), "bytes in one line", bounds.lineBytes)))
	}
	return line, nil
}

// appendBounded adds a line to a single member built in memory, failing at
// the line that would take it past the single-member bound.
func (w *Writer) appendBounded(name string, buf *bytes.Buffer, line []byte) error {
	if n := buf.Len() + len(line); n > bounds.singleBytes {
		return w.fail(fmt.Errorf("%s: %w", name, over(int64(n), "bytes in a single member", bounds.singleBytes)))
	}
	buf.Write(line)
	return nil
}

func (w *Writer) header(rel string, size int64) *tar.Header {
	return &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     w.stem + "/" + rel,
		Mode:     0o644,
		Size:     size,
		ModTime:  memberTime,
	}
}

// advance moves the writer to phase to, writing the buffered line members of
// every phase it leaves.
func (w *Writer) advance(to int, what string) error {
	if w.err != nil {
		return w.err
	}
	if w.open != nil {
		return w.fail(fmt.Errorf("%s while %s is still open: close it first", what, w.open.dir))
	}
	if to < w.phase {
		return w.fail(fmt.Errorf("%s is out of order: the layout puts it before what is already written", what))
	}
	for p := w.phase; p < to; p++ {
		if err := w.flushLines(p); err != nil {
			return err
		}
	}
	w.phase = to
	return nil
}

func (w *Writer) flushLines(phase int) error {
	name, ok := lineMembers[phase]
	if !ok {
		return nil
	}
	buf := w.lines[name]
	if buf == nil && !mandatoryMember(name) {
		return nil
	}
	var data []byte
	if buf != nil {
		data = buf.Bytes()
	}
	delete(w.lines, name)
	return w.member(name, data)
}

func (w *Writer) addLine(name string, line []byte) error {
	buf := w.lines[name]
	if buf == nil {
		buf = &bytes.Buffer{}
		w.lines[name] = buf
	}
	return w.appendBounded(name, buf, line)
}

// rowLine stamps and checks a row of member before it is written.
func (w *Writer) rowLine(member string, r Row) ([]byte, error) {
	if !w.declared[r.Concept] {
		return nil, w.fail(fmt.Errorf("%s: a row of %s, which the manifest does not declare", member, r.Concept))
	}
	if r.ID == "" {
		return nil, w.fail(fmt.Errorf("%s: a row of %s with no id", member, r.Concept))
	}
	r.Kind = KindRow
	return w.line(member, r)
}

// AccountRow adds one version of the account's own row to account.jsonl. The
// member holds that row and nothing else, and the checker refuses a file where
// it holds another kind (verify.go), so a row of another kind is refused here
// before a byte of it is written.
func (w *Writer) AccountRow(r Row) error {
	if r.Concept != accountKind {
		return w.fail(fmt.Errorf("%s: a row of %s, where only %s belongs", MemberAccount, r.Concept, accountKind))
	}
	if err := w.advance(phaseAccount, MemberAccount); err != nil {
		return err
	}
	line, err := w.rowLine(MemberAccount, r)
	if err != nil {
		return err
	}
	if err := w.addLine(MemberAccount, line); err != nil {
		return err
	}
	w.counts.row(&w.acctLast, r)
	return nil
}

// Line adds one line to a single member: people.jsonl, references.jsonl,
// connections.jsonl or blocks.jsonl.
func (w *Writer) Line(member string, v any) error {
	phase, ok := singlePhase[member]
	if _, isLine := lineMembers[phase]; !ok || !isLine || member == MemberAccount {
		return w.fail(fmt.Errorf("%q is not a line member", member))
	}
	if err := w.advance(phase, member); err != nil {
		return err
	}
	line, err := w.line(member, v)
	if err != nil {
		return err
	}
	if err := w.addLine(member, line); err != nil {
		return err
	}
	w.counts.members[member]++
	return nil
}

// Schema writes schemas/<fingerprint>.json. Each fingerprint once.
func (w *Writer) Schema(fingerprint string, doc json.RawMessage) error {
	rel := dirSchemas + "/" + fingerprint + ".json"
	if err := w.advance(phaseSchemas, rel); err != nil {
		return err
	}
	if !safeSegment(fingerprint) {
		return w.fail(fmt.Errorf("%q cannot name a schema", fingerprint))
	}
	if w.schemas[fingerprint] {
		return w.fail(fmt.Errorf("%s written twice", rel))
	}
	if !json.Valid(doc) {
		return w.fail(fmt.Errorf("%s is not JSON", rel))
	}
	w.schemas[fingerprint] = true
	return w.member(rel, doc)
}

// Bytes copies one object into bytes/NNNNNN and records its line in
// bytes.jsonl. size comes from the caller, because the tar header needs it
// before the bytes; a reader that yields fewer or more bytes fails the
// write, since either way the member would not be the object.
func (w *Writer) Bytes(meta BytesMeta, size int64, r io.Reader) error {
	if err := w.advance(phaseBytes, "bytes"); err != nil {
		return err
	}
	if meta.Key == "" || size < 0 || meta.Missing || meta.Refused {
		return w.fail(fmt.Errorf("bytes need a key and a size, and are neither missing nor refused"))
	}
	w.objects++
	rel := objectFile(w.objects)
	if err := w.reserveSum(rel); err != nil {
		return err
	}
	if err := w.tw.WriteHeader(w.header(rel, size)); err != nil {
		return w.fail(fmt.Errorf("%s: %w", rel, err))
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(w.tw, h), r, size)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return w.fail(fmt.Errorf("%s (%s): the reader is short: %d of %d bytes", rel, meta.Key, n, size))
		}
		return w.fail(fmt.Errorf("%s (%s): %w", rel, meta.Key, err))
	}
	var probe [1]byte
	if k, _ := io.ReadFull(r, probe[:]); k > 0 {
		return w.fail(fmt.Errorf("%s (%s): the reader is long: it has more than %d bytes", rel, meta.Key, size))
	}
	digest := hex.EncodeToString(h.Sum(nil))
	w.sums = append(w.sums, sumLine{path: rel, digest: digest})
	line, err := w.line(MemberBytesIndex, BytesLine{BytesMeta: meta, File: rel, Size: size, SHA256: digest})
	if err != nil {
		return err
	}
	if err := w.appendBounded(MemberBytesIndex, &w.bytesIndex, line); err != nil {
		return err
	}
	w.counts.objects++
	w.counts.objBytes += size
	return nil
}

// MissingBytes records an object a carried row points at that the source no
// longer holds. The run goes on: the source does not have them to give.
func (w *Writer) MissingBytes(meta BytesMeta) error {
	if err := w.advance(phaseBytes, "bytes"); err != nil {
		return err
	}
	if meta.Key == "" {
		return w.fail(errors.New("missing bytes need a key"))
	}
	if meta.Refused {
		return w.fail(errors.New("a reference is missing or refused, never both"))
	}
	meta.Missing = true
	line, err := w.line(MemberBytesIndex, BytesLine{BytesMeta: meta})
	if err != nil {
		return err
	}
	if err := w.appendBounded(MemberBytesIndex, &w.bytesIndex, line); err != nil {
		return err
	}
	w.counts.missing++
	return nil
}

// RefusedBytes records a reference a carried row makes to bytes outside the
// place its kind's declaration allows (design section B, "Bytes"). Nothing is
// read: the line names the row, the field and the key as the row wrote it,
// and no file. The run goes on, as for missing bytes.
func (w *Writer) RefusedBytes(meta BytesMeta) error {
	if err := w.advance(phaseBytes, "bytes"); err != nil {
		return err
	}
	if meta.Key == "" {
		return w.fail(errors.New("refused bytes need a key"))
	}
	if meta.Missing {
		return w.fail(errors.New("a reference is missing or refused, never both"))
	}
	meta.Refused = true
	line, err := w.line(MemberBytesIndex, BytesLine{BytesMeta: meta})
	if err != nil {
		return err
	}
	if err := w.appendBounded(MemberBytesIndex, &w.bytesIndex, line); err != nil {
		return err
	}
	w.counts.refused++
	return nil
}

// Finish writes bytes.jsonl, report.json, README.txt and SHA256SUMS and
// closes the tar. The report's written counts and its bytes counts must
// agree with what was written; a report that disagrees is refused rather
// than written, because Verify would refuse the file it made.
func (w *Writer) Finish(r Report) error {
	if err := w.advance(phaseBytesIndex, "finishing"); err != nil {
		return err
	}
	if err := checkReport(r, w.counts); err != nil {
		return w.fail(err)
	}
	r = normalizeReport(r)
	if err := w.member(MemberBytesIndex, w.bytesIndex.Bytes()); err != nil {
		return err
	}
	doc, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return w.fail(err)
	}
	if err := w.member(MemberReport, append(doc, '\n')); err != nil {
		return err
	}
	if err := w.member(MemberReadme, []byte(renderReadme(w.stem, w.m, r))); err != nil {
		return err
	}
	// SHA256SUMS lists every member before it, and not itself.
	sums := renderSums(w.sums)
	if err := w.tw.WriteHeader(w.header(MemberSums, int64(len(sums)))); err != nil {
		return w.fail(err)
	}
	if _, err := w.tw.Write(sums); err != nil {
		return w.fail(err)
	}
	if err := w.tw.Close(); err != nil {
		return w.fail(err)
	}
	w.phase = phaseSums
	w.err = errors.New("accountexport: the file is finished")
	return nil
}

// normalizeReport writes empty lists as [] rather than null, so a person
// reading report.json sees "none" and not "absent".
func normalizeReport(r Report) Report {
	if r.Kinds == nil {
		r.Kinds = []KindCount{}
	}
	if r.LeftOut == nil {
		r.LeftOut = []LeftOut{}
	}
	if r.InNoAccount == nil {
		r.InNoAccount = []KindRows{}
	}
	if r.ScrubbedFields == nil {
		r.ScrubbedFields = []ScrubbedField{}
	}
	if r.Rebuilt == nil {
		r.Rebuilt = []string{}
	}
	if r.Members == nil {
		r.Members = []MemberCount{}
	}
	return r
}

// Parted opens a folder of parts: rows/<namespace>/<name> for a kind (see
// RowsDir), tables/<name>/<table> for a hand-over. One folder is open at a
// time, each folder is opened once, and nothing else is written until it is
// closed. A refusal here is answered by the first Write or Close.
func (w *Writer) Parted(dir string) *PartWriter {
	p := &PartWriter{w: w, dir: dir}
	phase := phaseRows
	if len(dir) > len(dirTables) && dir[:len(dirTables)+1] == dirTables+"/" {
		phase = phaseTables
	}
	if err := w.advance(phase, dir); err != nil {
		p.err = err
		return p
	}
	switch {
	case !validDir(dir):
		p.err = w.fail(fmt.Errorf("%q is not a folder of the layout", dir))
	case w.dirs[dir]:
		p.err = w.fail(fmt.Errorf("%s opened twice", dir))
	case phase == phaseRows && !w.declared[conceptOfRowsDir(dir)]:
		p.err = w.fail(fmt.Errorf("%s is the folder of %s, which the manifest does not declare", dir, conceptOfRowsDir(dir)))
	}
	if p.err != nil {
		return p
	}
	if phase == phaseRows {
		p.concept = conceptOfRowsDir(dir)
	}
	w.dirs[dir] = true
	w.open = p
	return p
}

// PartWriter writes the lines of one folder, cutting a part at the bound.
type PartWriter struct {
	w       *Writer
	dir     string
	concept string // for a rows folder: every line is a Row of it
	cur     *part
	n       int
	last    string
	err     error
	closed  bool
}

// Write adds one JSON line. In a rows folder v is a Row (or *Row) of the
// folder's kind; its Kind is stamped here.
func (p *PartWriter) Write(v any) error {
	if p.err != nil {
		return p.err
	}
	if p.w.err != nil {
		return p.w.err
	}
	if p.closed || p.w.open != p {
		p.err = p.w.fail(fmt.Errorf("%s written after it was closed", p.dir))
		return p.err
	}
	var line []byte
	var err error
	var row Row
	if p.concept != "" {
		switch r := v.(type) {
		case Row:
			row = r
		case *Row:
			row = *r
		default:
			p.err = p.w.fail(fmt.Errorf("%s takes rows, not %T", p.dir, v))
			return p.err
		}
		if row.Concept != p.concept {
			p.err = p.w.fail(fmt.Errorf("%s: a row of %s in the folder of %s", p.dir, row.Concept, p.concept))
			return p.err
		}
		line, err = p.w.rowLine(p.dir, row)
	} else {
		line, err = p.w.line(p.dir, v)
	}
	if err != nil {
		p.err = err
		return p.err
	}
	if p.cur != nil && p.cur.full(len(line)) {
		if err := p.cut(); err != nil {
			return err
		}
	}
	if p.cur == nil {
		p.cur = newPart()
	}
	if err := p.cur.add(line); err != nil {
		p.err = p.w.fail(fmt.Errorf("%s: %w", p.dir, err))
		return p.err
	}
	if p.concept != "" {
		p.w.counts.row(&p.last, row)
	} else {
		p.w.counts.members[p.dir]++
	}
	return nil
}

func (p *PartWriter) cut() error {
	data, err := p.cur.close()
	if err != nil {
		p.err = p.w.fail(fmt.Errorf("%s: %w", p.dir, err))
		return p.err
	}
	p.n++
	p.cur = nil
	if err := p.w.member(p.dir+"/"+partFile(p.n), data); err != nil {
		p.err = err
		return err
	}
	return nil
}

// Close writes the last part. A folder with no lines writes no part.
func (p *PartWriter) Close() error {
	if p.err != nil {
		return p.err
	}
	if p.closed {
		return nil
	}
	if p.cur != nil {
		if err := p.cut(); err != nil {
			return err
		}
	}
	p.closed = true
	if p.w.open == p {
		p.w.open = nil
	}
	return p.w.err
}

// HandOverSink is the writer as a hand-over sees it: folders under tables/
// and the hand-overs' own single member. The run's own members are not
// reachable through it.
func (w *Writer) HandOverSink() HandOverSink { return handOverSink{w} }

type handOverSink struct{ w *Writer }

func (s handOverSink) Parted(dir string) LineSink { return s.w.Parted(dirTables + "/" + dir) }

func (s handOverSink) Line(member string, v any) error {
	if member != MemberBlocks {
		return s.w.fail(fmt.Errorf("a hand-over may not write %s", member))
	}
	return s.w.Line(member, v)
}

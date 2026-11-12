package accountexport

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

// VerifyResult is what Verify read.
type VerifyResult struct {
	Manifest Manifest
	Report   Report
	// Members is the number of members, SHA256SUMS included.
	Members int
	// Bytes is the length of the stream read, which for an unprotected file
	// is the file's size.
	Bytes int64
}

// Verify walks an unprotected tar stream: structure, every member against
// SHA256SUMS, and the report's written counts against the lines. A file with
// no SHA256SUMS is cut short. The manifest's formatVersion is checked before
// any other member is read, so a newer file is refused with ErrFormatTooNew
// whatever follows it.
func Verify(r io.Reader) (VerifyResult, error) {
	cr := &countingReader{r: r}
	v := &verifier{
		tr:       tar.NewReader(cr),
		counts:   newTally(),
		seen:     map[string]bool{},
		dirDone:  map[string]bool{},
		dirParts: map[string]int{},
		dirLast:  map[string]string{},
		objects:  map[string]object{},
	}
	if err := v.run(); err != nil {
		return VerifyResult{}, fmt.Errorf("accountexport: verify: %w", err)
	}
	// Count the end-of-archive blocks too, so Bytes is the whole stream.
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return VerifyResult{}, fmt.Errorf("accountexport: verify: %w", err)
	}
	return VerifyResult{Manifest: v.manifest, Report: v.report, Members: len(v.digests) + 1, Bytes: cr.n}, nil
}

type object struct {
	size   int64
	digest string
	used   bool
}

type verifier struct {
	tr       *tar.Reader
	stem     string
	manifest Manifest
	declared map[string]bool
	report   Report
	counts   *tally

	phase    int
	seen     map[string]bool // single members and schemas read
	openDir  string
	dirDone  map[string]bool
	dirParts map[string]int
	dirLast  map[string]string
	acctLast string
	objects  map[string]object
	lastObj  int
	digests  []sumLine
	sawSums  bool
}

func (v *verifier) next() (*tar.Header, error) {
	h, err := v.tr.Next()
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, errors.New("the file is cut short inside a member")
	}
	return h, err
}

func (v *verifier) run() error {
	h, err := v.next()
	if errors.Is(err, io.EOF) {
		return errors.New("the stream holds no member: not an account export")
	}
	if err != nil {
		return err
	}
	stem, rel, ok := strings.Cut(h.Name, "/")
	if !ok || rel != MemberManifest || !safeSegment(stem) {
		return fmt.Errorf("the first member is %q: an account export opens with <folder>/%s", h.Name, MemberManifest)
	}
	v.stem = stem
	if err := v.readManifest(h); err != nil {
		return fmt.Errorf("%s: %w", MemberManifest, err)
	}

	for {
		h, err := v.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if v.sawSums {
			return fmt.Errorf("%s follows %s, which is last", h.Name, MemberSums)
		}
		rel, err := v.place(h)
		if err != nil {
			return err
		}
		if err := v.read(rel, h); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	if !v.sawSums {
		return fmt.Errorf("the file is cut short: it has no %s, which is written last", MemberSums)
	}
	for _, name := range []string{MemberAccount, MemberPeople, MemberReferences, MemberConnections, MemberBytesIndex, MemberReport, MemberReadme} {
		if !v.seen[name] {
			return fmt.Errorf("the file has no %s", name)
		}
	}
	for name, o := range v.objects {
		if !o.used {
			return fmt.Errorf("%s: %s does not name it", name, MemberBytesIndex)
		}
	}
	if err := checkReport(v.report, v.counts); err != nil {
		return fmt.Errorf("%s: %w", MemberReport, err)
	}
	return nil
}

func (v *verifier) readManifest(h *tar.Header) error {
	if h.Size > int64(bounds.manifestBytes) {
		return over(h.Size, "bytes in the manifest", bounds.manifestBytes)
	}
	data, err := v.readAll(MemberManifest, h)
	if err != nil {
		return err
	}
	// "Too new" is decided first, from a struct holding nothing but the
	// version: a later format may change what any other field holds, and
	// such a file must read as too new, never as malformed.
	var version struct {
		FormatVersion int `json:"formatVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return fmt.Errorf("no formatVersion a reader can take: %w", err)
	}
	if err := checkFormatVersion(version.FormatVersion); err != nil {
		return err
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &kind); err != nil || kind.Kind != KindManifest {
		return fmt.Errorf("kind is not %q: not an account export", KindManifest)
	}
	if err := json.Unmarshal(data, &v.manifest); err != nil {
		return err
	}
	v.declared = map[string]bool{}
	for _, k := range v.manifest.Kinds {
		v.declared[k.Concept] = true
	}
	return nil
}

// place checks a member's header and its place in the layout.
func (v *verifier) place(h *tar.Header) (string, error) {
	if h.Typeflag != tar.TypeReg {
		return "", fmt.Errorf("%s is not a regular file", h.Name)
	}
	stem, rel, ok := strings.Cut(h.Name, "/")
	if !ok || stem != v.stem {
		return "", fmt.Errorf("%s is outside the folder %s", h.Name, v.stem)
	}
	pm, ok := parseMember(rel)
	if !ok || pm.phase == phaseManifest {
		return "", fmt.Errorf("%s is not a member of the layout", rel)
	}
	if pm.phase < v.phase {
		return "", fmt.Errorf("%s is out of order", rel)
	}
	v.phase = pm.phase
	if pm.dir != "" {
		if pm.dir != v.openDir {
			if v.dirDone[pm.dir] {
				return "", fmt.Errorf("%s: the parts of %s are not together", rel, pm.dir)
			}
			if v.openDir != "" {
				v.dirDone[v.openDir] = true
			}
			v.openDir = pm.dir
		}
		if pm.part != v.dirParts[pm.dir]+1 {
			return "", fmt.Errorf("%s: expected part %d", rel, v.dirParts[pm.dir]+1)
		}
		v.dirParts[pm.dir] = pm.part
		if pm.phase == phaseRows {
			c := conceptOfRowsDir(pm.dir)
			if !v.declared[c] {
				return "", fmt.Errorf("%s holds %s, which the manifest does not declare", rel, c)
			}
		}
		return rel, nil
	}
	if v.openDir != "" {
		v.dirDone[v.openDir] = true
		v.openDir = ""
	}
	if pm.obj > 0 {
		if pm.obj != v.lastObj+1 {
			return "", fmt.Errorf("%s: expected %s", rel, objectFile(v.lastObj+1))
		}
		v.lastObj = pm.obj
		return rel, nil
	}
	if v.seen[rel] {
		return "", fmt.Errorf("%s appears twice", rel)
	}
	v.seen[rel] = true
	return rel, nil
}

// hashed reads a member's body, hashing everything it reads, and records
// the digest once the body is read to its end.
type hashed struct {
	r io.Reader
	h hash.Hash
}

func (x *hashed) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	x.h.Write(p[:n])
	return n, err
}

func (v *verifier) open() *hashed { return &hashed{r: v.tr, h: sha256.New()} }

func (v *verifier) done(rel string, x *hashed) (string, error) {
	// Drain what a parser left, so the digest is of the whole member.
	if _, err := io.Copy(io.Discard, x); err != nil {
		return "", v.cut(err)
	}
	d := hex.EncodeToString(x.h.Sum(nil))
	v.digests = append(v.digests, sumLine{path: rel, digest: d})
	return d, nil
}

func (v *verifier) cut(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the file is cut short inside this member")
	}
	return err
}

func (v *verifier) readAll(rel string, h *tar.Header) ([]byte, error) {
	x := v.open()
	data, err := io.ReadAll(x)
	if err != nil {
		return nil, v.cut(err)
	}
	if _, err := v.done(rel, x); err != nil {
		return nil, err
	}
	return data, nil
}

func (v *verifier) read(rel string, h *tar.Header) error {
	pm, _ := parseMember(rel)
	// Every member but a bytes/ object is held to its bound from the header,
	// before a byte of it is read. An object is streamed and only hashed.
	if pm.phase != phaseBytes {
		if bound, what := memberBound(rel); h.Size > int64(bound) {
			return over(h.Size, what, bound)
		}
	}
	switch {
	case pm.phase == phaseRows || pm.phase == phaseTables:
		return v.readPart(rel, pm)
	case pm.phase == phaseBytes:
		x := v.open()
		n, err := io.Copy(io.Discard, x)
		if err != nil {
			return v.cut(err)
		}
		d, err := v.done(rel, x)
		if err != nil {
			return err
		}
		v.objects[rel] = object{size: n, digest: d}
		return nil
	case pm.phase == phaseSchemas:
		data, err := v.readAll(rel, h)
		if err != nil {
			return err
		}
		if !json.Valid(data) {
			return errors.New("not JSON")
		}
		return nil
	case pm.phase == phaseSums:
		v.sawSums = true
		return v.checkSums(bufio.NewReader(v.tr))
	}
	data, err := v.readAll(rel, h)
	if err != nil {
		return err
	}
	switch rel {
	case MemberAccount:
		return eachLine(data, func(line []byte) error {
			row, err := v.row(line)
			if err != nil {
				return err
			}
			v.counts.row(&v.acctLast, row)
			return nil
		})
	case MemberPeople, MemberReferences, MemberConnections, MemberBlocks:
		return eachLine(data, func(line []byte) error {
			if !json.Valid(line) {
				return errors.New("a line is not JSON")
			}
			v.counts.members[rel]++
			return nil
		})
	case MemberBytesIndex:
		return eachLine(data, v.bytesLine)
	case MemberReport:
		return json.Unmarshal(data, &v.report)
	}
	return nil // README.txt
}

func (v *verifier) row(line []byte) (Row, error) {
	var row Row
	if err := json.Unmarshal(line, &row); err != nil {
		return row, fmt.Errorf("a row line is not a row: %w", err)
	}
	if row.Kind != KindRow {
		return row, fmt.Errorf("a line of kind %q where a row was expected", row.Kind)
	}
	if !v.declared[row.Concept] {
		return row, fmt.Errorf("a row of %s, which the manifest does not declare", row.Concept)
	}
	if row.ID == "" {
		return row, fmt.Errorf("a row of %s with no id", row.Concept)
	}
	return row, nil
}

func (v *verifier) readPart(rel string, pm parsedMember) error {
	x := v.open()
	zr, err := gzip.NewReader(x)
	if err != nil {
		return v.cut(err)
	}
	// One part is one gzip member.
	zr.Multistream(false)
	// At most one byte past the decompressed bound is read, which is how a
	// part that inflates past it is told from one that ends at it.
	raw := &countingReader{r: io.LimitReader(zr, int64(bounds.partRawBytes)+1)}
	br := bufio.NewReader(raw)
	concept := conceptOfRowsDir(pm.dir)
	lines := 0
	for {
		line, err := readLine(br, bounds.lineBytes)
		if raw.n > int64(bounds.partRawBytes) {
			return over(raw.n, "bytes decompressed from a part, or more,", bounds.partRawBytes)
		}
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				return errors.New("the last line has no newline")
			}
			if lines++; lines > bounds.partLines {
				return over(int64(lines), "lines in a part, or more,", bounds.partLines)
			}
			if pm.phase == phaseRows {
				row, rerr := v.row(line)
				if rerr != nil {
					return rerr
				}
				if row.Concept != concept {
					return fmt.Errorf("a row of %s in the folder of %s", row.Concept, concept)
				}
				last := v.dirLast[pm.dir]
				v.counts.row(&last, row)
				v.dirLast[pm.dir] = last
			} else {
				if !json.Valid(line) {
					return errors.New("a line is not JSON")
				}
				v.counts.members[pm.dir]++
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return v.cut(err)
		}
	}
	if err := zr.Close(); err != nil {
		return err
	}
	// Anything after the gzip member's trailer is not part of the format.
	var probe [1]byte
	if n, _ := io.ReadFull(x, probe[:]); n > 0 {
		return errors.New("bytes follow the part's gzip member")
	}
	_, err = v.done(rel, x)
	return err
}

func (v *verifier) bytesLine(line []byte) error {
	var b BytesLine
	if err := json.Unmarshal(line, &b); err != nil {
		return err
	}
	if b.Key == "" {
		return errors.New("a line names no key")
	}
	if b.Refused {
		if b.Missing {
			return fmt.Errorf("%s is recorded as both missing and refused", b.Key)
		}
		if b.File != "" || b.Size != 0 || b.SHA256 != "" {
			return fmt.Errorf("%s was refused and names a file, a size or a checksum", b.Key)
		}
		v.counts.refused++
		return nil
	}
	if b.Missing {
		if b.File != "" {
			return fmt.Errorf("%s is missing at the source and names a file", b.Key)
		}
		v.counts.missing++
		return nil
	}
	o, ok := v.objects[b.File]
	if !ok {
		return fmt.Errorf("%s names %q, which the file does not hold", b.Key, b.File)
	}
	if o.used {
		return fmt.Errorf("%s is named twice", b.File)
	}
	if o.size != b.Size || o.digest != b.SHA256 {
		return fmt.Errorf("%s: its size or SHA-256 is not what this line records for %s", b.File, b.Key)
	}
	o.used = true
	v.objects[b.File] = o
	v.counts.objects++
	v.counts.objBytes += o.size
	return nil
}

// checkSums reads SHA256SUMS as a stream and holds every member read to
// its line, in the order the members came: the writer lists them in that
// order, and reading line by line keeps the list out of memory.
func (v *verifier) checkSums(br *bufio.Reader) error {
	n := 0
	for {
		line, err := readLine(br, sumLineBytes(strings.Repeat("x", relMaxBytes)))
		if len(line) > 0 {
			got, perr := parseSumLine(line)
			if perr != nil {
				return fmt.Errorf("line %d: %w", n+1, perr)
			}
			if n >= len(v.digests) {
				return fmt.Errorf("lists %s, which the file does not hold", got.path)
			}
			want := v.digests[n]
			if got.path != want.path {
				return fmt.Errorf("line %d lists %s where the file holds %s", n+1, got.path, want.path)
			}
			if got.digest != want.digest {
				return fmt.Errorf("%s does not match its line: the member was changed", want.path)
			}
			n++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return v.cut(err)
		}
	}
	if n < len(v.digests) {
		return fmt.Errorf("%s is not listed", v.digests[n].path)
	}
	return nil
}

// readLine reads one line, its newline included, refusing one longer than
// max before reading past it. At the end of the input it answers what is
// left with io.EOF.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, over(int64(len(line)+len(chunk)), "bytes in one line, or more,", max)
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// eachLine calls fn for each newline-ended line, held to the line bound. A
// member that does not end in a newline was cut or changed.
func eachLine(data []byte, fn func([]byte) error) error {
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return errors.New("the last line has no newline")
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i+1 > bounds.lineBytes {
			return over(int64(i+1), "bytes in one line", bounds.lineBytes)
		}
		if err := fn(data[:i]); err != nil {
			return err
		}
		data = data[i+1:]
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

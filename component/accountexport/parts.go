package accountexport

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// limits are every bound of the format. The writer and Verify both read
// them, so a file the writer finishes is always within what Verify accepts.
type limits struct {
	partBytes, partLines, partRawBytes, partStoredBytes int
	lineBytes, singleBytes, manifestBytes, schemaBytes  int
	sumsBytes                                           int
}

// The bounds. Verify will read files someone outside could have edited, so
// every read it makes is bounded by one of these, and the writer refuses to
// write past any of them.
const (
	// partMaxBytes is where a part is cut by its compressed size (spec D).
	partMaxBytes = 8 << 20
	// partMaxLines is where a part is cut by its line count (spec D).
	partMaxLines = 50000
	// lineMaxBytes bounds one JSON line. It is far above any row payload the
	// engine writes and small enough to hold one line in memory; a line
	// larger than partMaxBytes is a part of its own, so this, not
	// partMaxBytes, sizes the largest legal part.
	lineMaxBytes = 64 << 20
	// partMaxStoredBytes bounds a part's size in the tar: one line of
	// lineMaxBytes that does not compress, plus 1 MiB, which covers deflate's
	// framing (five bytes per 64 KiB block, plus the flushes) many times over.
	partMaxStoredBytes = lineMaxBytes + 1<<20
	// partMaxRawBytes bounds a part's decompressed size, so a small part
	// cannot inflate without end; it is four of the largest lines, and the
	// writer cuts a part before passing it.
	partMaxRawBytes = 256 << 20
	// singleMaxBytes bounds a single member (account.jsonl, people.jsonl,
	// bytes.jsonl, report.json, ...), which is built in memory because a tar
	// member needs its size before its bytes (spec D).
	singleMaxBytes = 256 << 20
	// manifestMaxBytes bounds the manifest, read whole before anything is
	// known about the file; its kinds and language lines are a few KiB.
	manifestMaxBytes = 16 << 20
	// schemaMaxBytes bounds one schema document; a concept's schema is tens
	// of KiB, so this leaves two orders of magnitude.
	schemaMaxBytes = 4 << 20
	// sumsMaxBytes bounds SHA256SUMS: at most about 320 bytes a line (a
	// digest, two spaces, a path of at most relMaxBytes), it fits more than
	// three million members.
	sumsMaxBytes = 1 << 30
	// relMaxBytes bounds a member's path inside the folder, which keeps each
	// line of SHA256SUMS bounded.
	relMaxBytes = 255
)

// bounds is a variable holding the constants above only so a test can reach
// a bound without writing megabytes.
var bounds = limits{
	partBytes:       partMaxBytes,
	partLines:       partMaxLines,
	partRawBytes:    partMaxRawBytes,
	partStoredBytes: partMaxStoredBytes,
	lineBytes:       lineMaxBytes,
	singleBytes:     singleMaxBytes,
	manifestBytes:   manifestMaxBytes,
	schemaBytes:     schemaMaxBytes,
	sumsBytes:       sumsMaxBytes,
}

// over is the one wording for a refused bound. The caller names the member.
func over(n int64, what string, bound int) error {
	return fmt.Errorf("%d %s is above the bound of %d", n, what, bound)
}

// memberBound answers the bound on a whole member's size, by its place in
// the layout. A bytes/ object is bounded by the size its row gives, not
// here; SHA256SUMS is bounded by bounds.sumsBytes.
func memberBound(rel string) (int, string) {
	pm, _ := parseMember(rel)
	switch {
	case pm.phase == phaseManifest:
		return bounds.manifestBytes, "bytes in the manifest"
	case pm.phase == phaseSchemas:
		return bounds.schemaBytes, "bytes in a schema"
	case pm.phase == phaseRows || pm.phase == phaseTables:
		return bounds.partStoredBytes, "bytes in a part"
	case pm.phase == phaseSums:
		return bounds.sumsBytes, "bytes in the checksum list"
	}
	return bounds.singleBytes, "bytes in a single member"
}

// sumLineBytes is the length of a member's line in SHA256SUMS.
func sumLineBytes(rel string) int { return sha256HexLen + 2 + len(rel) + 1 }

const sha256HexLen = 64

// gzipLevel is fixed: the same lines must give the same bytes, and a level
// is part of what the bytes depend on.
const gzipLevel = 6

// flushEvery bounds how much input the compressor may hold unreported, so
// the size estimate below stays an upper bound without flushing every line.
const flushEvery = 1 << 20

// gzipSlack covers the gzip header and trailer and the block framing of
// what is still pending, so a cut part never exceeds partMaxBytes because of
// framing.
const gzipSlack = 256

// The layout's member order (spec D). A member's phase is its place in the
// order; the writer and Verify both refuse a member whose phase is below the
// previous one's.
const (
	phaseManifest = iota
	phaseAccount
	phaseRows
	phaseSchemas
	phasePeople
	phaseReferences
	phaseConnections
	phaseBlocks
	phaseTables
	phaseBytes
	phaseBytesIndex
	phaseReport
	phaseReadme
	phaseSums
)

var singlePhase = map[string]int{
	MemberManifest:    phaseManifest,
	MemberAccount:     phaseAccount,
	MemberPeople:      phasePeople,
	MemberReferences:  phaseReferences,
	MemberConnections: phaseConnections,
	MemberBlocks:      phaseBlocks,
	MemberBytesIndex:  phaseBytesIndex,
	MemberReport:      phaseReport,
	MemberReadme:      phaseReadme,
	MemberSums:        phaseSums,
}

// lineMembers are the single members built from Line calls, by phase.
var lineMembers = map[int]string{
	phaseAccount:     MemberAccount,
	phasePeople:      MemberPeople,
	phaseReferences:  MemberReferences,
	phaseConnections: MemberConnections,
	phaseBlocks:      MemberBlocks,
}

// mandatoryMember is a single member every file has, empty when nothing was
// written to it, so a reader never has to tell "none" from "lost".
// blocks.jsonl is a hand-over's and is present only when one wrote to it.
func mandatoryMember(name string) bool {
	switch name {
	case MemberAccount, MemberPeople, MemberReferences, MemberConnections:
		return true
	}
	return false
}

var (
	partName   = regexp.MustCompile(`^part-([0-9]{6})\.jsonl\.gz$`)
	objectName = regexp.MustCompile(`^[0-9]{6}$`)
)

func partFile(n int) string   { return fmt.Sprintf("part-%06d.jsonl.gz", n) }
func objectFile(n int) string { return fmt.Sprintf("%s/%06d", dirBytes, n) }

// parsedMember is what a member's path says about it.
type parsedMember struct {
	phase int
	dir   string // rows/<ns>/<name> or tables/<a>/<b>, for a part
	part  int    // the part's number, from 1
	obj   int    // the object's number, for bytes/NNNNNN
}

// parseMember places a path relative to the folder in the layout.
func parseMember(rel string) (parsedMember, bool) {
	if p, ok := singlePhase[rel]; ok {
		return parsedMember{phase: p}, true
	}
	seg := strings.Split(rel, "/")
	for _, s := range seg {
		if !safeSegment(s) {
			return parsedMember{}, false
		}
	}
	switch {
	case len(seg) == 4 && (seg[0] == dirRows || seg[0] == dirTables):
		m := partName.FindStringSubmatch(seg[3])
		if m == nil {
			return parsedMember{}, false
		}
		n, _ := strconv.Atoi(m[1])
		phase := phaseRows
		if seg[0] == dirTables {
			phase = phaseTables
		}
		return parsedMember{phase: phase, dir: strings.Join(seg[:3], "/"), part: n}, n > 0
	case len(seg) == 2 && seg[0] == dirSchemas && strings.HasSuffix(seg[1], ".json") && safeSegment(strings.TrimSuffix(seg[1], ".json")):
		return parsedMember{phase: phaseSchemas}, true
	case len(seg) == 2 && seg[0] == dirBytes && objectName.MatchString(seg[1]):
		n, _ := strconv.Atoi(seg[1])
		return parsedMember{phase: phaseBytes, obj: n}, n > 0
	}
	return parsedMember{}, false
}

// validDir reports whether dir is a folder Parted may open: rows/<ns>/<name>
// or tables/<name>/<table>.
func validDir(dir string) bool {
	seg := strings.Split(dir, "/")
	if len(seg) != 3 || (seg[0] != dirRows && seg[0] != dirTables) {
		return false
	}
	return safeSegment(seg[1]) && safeSegment(seg[2])
}

// conceptOfRowsDir is RowsDir's inverse.
func conceptOfRowsDir(dir string) string {
	seg := strings.Split(dir, "/")
	if len(seg) != 3 || seg[0] != dirRows {
		return ""
	}
	return "v1:" + seg[1] + ":" + seg[2]
}

// encodeLine is one JSON line. HTML escaping is off so a payload's strings
// keep their characters; raw JSON is compacted by the encoder, which changes
// no value.
func encodeLine(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// part is one gzip member being built in memory.
type part struct {
	buf        bytes.Buffer
	zw         *gzip.Writer
	lines      int
	raw        int
	sinceFlush int
}

func newPart() *part {
	p := &part{}
	// The header is left zero: no time, no name, no comment. Go writes OS
	// 255 (unknown), which is fixed too.
	p.zw, _ = gzip.NewWriterLevel(&p.buf, gzipLevel)
	return p
}

// estimate is an upper bound on the part's size if it were closed now.
func (p *part) estimate() int { return p.buf.Len() + p.sinceFlush + gzipSlack }

// full reports whether adding a line of n bytes must cut the part first. An
// empty part takes any line, so a line larger than the bound is a part of
// its own.
func (p *part) full(n int) bool {
	if p.lines == 0 {
		return false
	}
	return p.lines >= bounds.partLines || p.estimate()+n > bounds.partBytes || p.raw+n > bounds.partRawBytes
}

func (p *part) add(line []byte) error {
	if _, err := p.zw.Write(line); err != nil {
		return err
	}
	p.lines++
	p.raw += len(line)
	p.sinceFlush += len(line)
	if p.sinceFlush >= flushEvery {
		if err := p.zw.Flush(); err != nil {
			return err
		}
		p.sinceFlush = 0
	}
	return nil
}

func (p *part) close() ([]byte, error) {
	if err := p.zw.Close(); err != nil {
		return nil, err
	}
	return p.buf.Bytes(), nil
}

// tally is what a file holds, counted the same way by the writer as it
// writes and by Verify as it reads, so the report can be held to both.
type tally struct {
	kinds    map[string]*kindTally
	members  map[string]int64
	objects  int64
	objBytes int64
	missing  int64
	refused  int64
}

type kindTally struct{ rows, versions int64 }

func newTally() *tally {
	return &tally{kinds: map[string]*kindTally{}, members: map[string]int64{}}
}

// row counts one version. Lines are ordered by id, so a row is a change of
// id from the line before in the same member or folder; that holds whatever
// collation ordered them.
func (t *tally) row(lastID *string, r Row) {
	k := t.kinds[r.Concept]
	if k == nil {
		k = &kindTally{}
		t.kinds[r.Concept] = k
	}
	k.versions++
	if r.ID != *lastID {
		k.rows++
		*lastID = r.ID
	}
}

// checkReport holds a report to what the file holds.
func checkReport(r Report, t *tally) error {
	seen := map[string]bool{}
	for _, k := range r.Kinds {
		if seen[k.Concept] {
			return fmt.Errorf("report names %s twice", k.Concept)
		}
		seen[k.Concept] = true
		got := t.kinds[k.Concept]
		if got == nil {
			got = &kindTally{}
		}
		if k.WrittenRows != got.rows || k.WrittenVersions != got.versions {
			return fmt.Errorf("report says %s has %d rows and %d versions written, the file holds %d and %d",
				k.Concept, k.WrittenRows, k.WrittenVersions, got.rows, got.versions)
		}
	}
	for concept, got := range t.kinds {
		if got.versions > 0 && !seen[concept] {
			return fmt.Errorf("the file holds %d versions of %s and the report does not count it", got.versions, concept)
		}
	}
	listed := map[string]bool{}
	for _, m := range r.Members {
		if listed[m.Name] {
			return fmt.Errorf("report names member %s twice", m.Name)
		}
		listed[m.Name] = true
		if got := t.members[m.Name]; got != m.Lines {
			return fmt.Errorf("report says %s has %d lines written, the file holds %d", m.Name, m.Lines, got)
		}
	}
	for name, got := range t.members {
		if got > 0 && !listed[name] {
			return fmt.Errorf("the file holds %d lines in %s and the report does not count them", got, name)
		}
	}
	for _, l := range r.LeftOut {
		switch l.Reason {
		case LeftOutSignin, LeftOutTransit, LeftOutMachine:
		default:
			return fmt.Errorf("report leaves out %s for a reason that is not one of signin, transit, machine: %q", l.Concept, l.Reason)
		}
	}
	b := r.Bytes
	if b.Objects != t.objects || b.Bytes != t.objBytes || b.Missing != t.missing {
		return fmt.Errorf("report says %d objects of %d bytes written and %d missing, the file holds %d of %d and %d missing",
			b.Objects, b.Bytes, b.Missing, t.objects, t.objBytes, t.missing)
	}
	if b.Refused != t.refused {
		return fmt.Errorf("report says %d bytes references refused, the file records %d", b.Refused, t.refused)
	}
	return nil
}

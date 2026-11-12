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

// The bounds a part is cut at (spec D). Variables, not constants, only so a
// test can cut parts without writing megabytes.
var (
	partMaxBytes = 8 << 20
	partMaxLines = 50000
)

// singleMaxBytes bounds a single-member .jsonl file. It is built in memory
// because a tar member needs its size before its bytes.
const singleMaxBytes = 256 << 20

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
	return p.lines >= partMaxLines || p.estimate()+n > partMaxBytes
}

func (p *part) add(line []byte) error {
	if _, err := p.zw.Write(line); err != nil {
		return err
	}
	p.lines++
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
	return nil
}

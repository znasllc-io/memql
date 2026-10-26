package githubapp

import (
	"context"
	"crypto/sha1" // Git object identity, not authorization.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	ReviewProjectionFormat       = "memql.complete-review-hunks.v1"
	ReviewProjectionAlgorithm    = "patience-unique-lis.v1"
	ReviewProjectionPromptMarker = "Input representation: " + ReviewProjectionFormat
	ReviewProjectionContext      = 8
	ReviewProjectionMaxBytes     = 512 << 10
	reviewProjectionMaxLines     = 131072
)

// ReviewProjection is model input, not a replacement for ReviewDiff authority.
// Every changed leaf remains in Files, including empty and mode-only changes.
// Text preserves CRLF and missing final newlines. Equal omitted intervals are
// independently checked against both complete source versions before capture.
type ReviewProjection struct {
	Format       string                `json:"format"`
	Algorithm    string                `json:"algorithm"`
	ContextLines int                   `json:"contextLines"`
	SourceDigest string                `json:"sourceDigest"`
	BeforeSHA    string                `json:"beforeSha"`
	AfterSHA     string                `json:"afterSha"`
	BeforeTree   string                `json:"beforeTree"`
	AfterTree    string                `json:"afterTree"`
	Files        []ReviewProjectedFile `json:"files"`
}
type ReviewProjectedVersion struct {
	Present bool   `json:"present"`
	Mode    string `json:"mode"`
	BlobSHA string `json:"blobSha"`
	Bytes   int    `json:"bytes"`
	Lines   int    `json:"lines"`
}
type ReviewProjectedFile struct {
	Path   string                 `json:"path"`
	Before ReviewProjectedVersion `json:"before"`
	After  ReviewProjectedVersion `json:"after"`
	Hunks  []ReviewHunk           `json:"hunks"`
}
type ReviewHunk struct {
	BeforeStart int              `json:"beforeStart"`
	BeforeCount int              `json:"beforeCount"`
	AfterStart  int              `json:"afterStart"`
	AfterCount  int              `json:"afterCount"`
	Lines       []ReviewHunkLine `json:"lines"`
}
type ReviewHunkLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

func reviewLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
func projectionVersion(v ReviewFileVersion) (ReviewProjectedVersion, error) {
	out := ReviewProjectedVersion{Present: v.Present, Mode: v.Mode, BlobSHA: v.BlobSHA, Bytes: len(v.Content)}
	if !v.Present {
		if v != (ReviewFileVersion{}) {
			return out, errors.New("absent review file contains evidence")
		}
		return out, nil
	}
	if (v.Mode != "100644" && v.Mode != "100755") || len(v.Content) > 1<<20 || !utf8.ValidString(v.Content) || strings.IndexByte(v.Content, 0) >= 0 {
		return out, errors.New("invalid complete review file")
	}
	h := sha1.New()
	fmt.Fprintf(h, "blob %d%c", len(v.Content), 0)
	h.Write([]byte(v.Content))
	if hex.EncodeToString(h.Sum(nil)) != v.BlobSHA {
		return out, errors.New("review projection source blob mismatch")
	}
	out.Lines = strings.Count(v.Content, "\n")
	if v.Content != "" && !strings.HasSuffix(v.Content, "\n") {
		out.Lines++
	}
	return out, nil
}

// reviewEdits uses unique-line anchors and a deterministic increasing
// subsequence. Each gap preserves its common prefix/suffix and replaces the
// entire remaining interval. Repeated lines never trigger quadratic search,
// recursion, heuristic omission or a provider patch fallback. A conservative
// replacement may expose extra unchanged text; the output bound still applies.
func reviewEdits(before, after []string) []ReviewHunkLine {
	type occurrence struct{ count, pos int }
	old, newer := map[string]occurrence{}, map[string]occurrence{}
	for i, s := range before {
		v := old[s]
		v.count++
		v.pos = i
		old[s] = v
	}
	for i, s := range after {
		v := newer[s]
		v.count++
		v.pos = i
		newer[s] = v
	}
	type anchor struct{ a, b int }
	candidates := make([]anchor, 0)
	for i, s := range before {
		if old[s].count == 1 && newer[s].count == 1 {
			candidates = append(candidates, anchor{i, newer[s].pos})
		}
	}
	tails, previous := make([]int, 0), make([]int, len(candidates))
	for i, a := range candidates {
		p := sort.Search(len(tails), func(j int) bool { return candidates[tails[j]].b >= a.b })
		previous[i] = -1
		if p > 0 {
			previous[i] = tails[p-1]
		}
		if p == len(tails) {
			tails = append(tails, i)
		} else {
			tails[p] = i
		}
	}
	anchors := make([]anchor, len(tails))
	if len(tails) > 0 {
		at := tails[len(tails)-1]
		for i := len(anchors) - 1; i >= 0; i-- {
			anchors[i] = candidates[at]
			at = previous[at]
		}
	}
	edits := make([]ReviewHunkLine, 0, len(before)+len(after))
	appendLines := func(op string, lines []string) {
		for _, line := range lines {
			edits = append(edits, ReviewHunkLine{op, line})
		}
	}
	gap := func(a, b []string) {
		p := 0
		for p < len(a) && p < len(b) && a[p] == b[p] {
			p++
		}
		suffix := 0
		for suffix < len(a)-p && suffix < len(b)-p && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
			suffix++
		}
		appendLines(" ", a[:p])
		appendLines("-", a[p:len(a)-suffix])
		appendLines("+", b[p:len(b)-suffix])
		appendLines(" ", a[len(a)-suffix:])
	}
	ai, bi := 0, 0
	for _, a := range anchors {
		gap(before[ai:a.a], after[bi:a.b])
		edits = append(edits, ReviewHunkLine{" ", before[a.a]})
		ai, bi = a.a+1, a.b+1
	}
	gap(before[ai:], after[bi:])
	return edits
}
func reviewHunks(edits []ReviewHunkLine) []ReviewHunk {
	type interval struct{ start, end int }
	intervals := make([]interval, 0)
	for i, line := range edits {
		if line.Op == " " {
			continue
		}
		s, e := max(0, i-ReviewProjectionContext), min(len(edits), i+1+ReviewProjectionContext)
		if len(intervals) > 0 && s <= intervals[len(intervals)-1].end {
			intervals[len(intervals)-1].end = e
		} else {
			intervals = append(intervals, interval{s, e})
		}
	}
	hunks := make([]ReviewHunk, 0, len(intervals))
	a, b, at := 0, 0, 0
	for _, span := range intervals {
		for at < span.start {
			if edits[at].Op != "+" {
				a++
			}
			if edits[at].Op != "-" {
				b++
			}
			at++
		}
		h := ReviewHunk{BeforeStart: a + 1, AfterStart: b + 1, Lines: append([]ReviewHunkLine(nil), edits[span.start:span.end]...)}
		for at < span.end {
			if edits[at].Op != "+" {
				a++
				h.BeforeCount++
			}
			if edits[at].Op != "-" {
				b++
				h.AfterCount++
			}
			at++
		}
		hunks = append(hunks, h)
	}
	return hunks
}

// verifyReviewHunks reconstructs AFTER from BEFORE, all displayed edits and
// equal omitted spans. It refuses missing edits, reordered/overlapping spans,
// bad counters, invented context and newline changes. No digest alone stands
// in for reconstruction.
func verifyReviewHunks(before, after []string, hunks []ReviewHunk) error {
	a, b := 0, 0
	consume := func(text string, side []string, at *int) bool {
		if *at >= len(side) || side[*at] != text {
			return false
		}
		*at++
		return true
	}
	for _, h := range hunks {
		startA, startB := h.BeforeStart-1, h.AfterStart-1
		if startA < a || startB < b || startA > len(before) || startB > len(after) || startA-a != startB-b {
			return errors.New("review hunk position mismatch")
		}
		for a < startA {
			if before[a] != after[b] {
				return errors.New("review projection omits changed bytes")
			}
			a++
			b++
		}
		aa, bb := a, b
		changed := false
		for _, line := range h.Lines {
			switch line.Op {
			case " ":
				if !consume(line.Text, before, &a) || !consume(line.Text, after, &b) {
					return errors.New("review context mismatch")
				}
			case "-":
				changed = true
				if !consume(line.Text, before, &a) {
					return errors.New("review removal mismatch")
				}
			case "+":
				changed = true
				if !consume(line.Text, after, &b) {
					return errors.New("review addition mismatch")
				}
			default:
				return errors.New("unknown review edit operation")
			}
		}
		if !changed || a-aa != h.BeforeCount || b-bb != h.AfterCount {
			return errors.New("review hunk extent mismatch")
		}
	}
	if len(before)-a != len(after)-b {
		return errors.New("review projection omits final changes")
	}
	for a < len(before) {
		if before[a] != after[b] {
			return errors.New("review projection omits final changed bytes")
		}
		a++
		b++
	}
	return nil
}

// ProjectReviewDiff has bounded source bytes, line inventory, O(n log n) work,
// and serialized output. Unsupported/oversized evidence refuses in Capture,
// before the durable model-attempt bit, with no source slice or model request.
func ProjectReviewDiff(ctx context.Context, d ReviewDiff) (ReviewProjection, error) {
	var out ReviewProjection
	if !branchChangeSHA.MatchString(d.BeforeSHA) || !branchChangeSHA.MatchString(d.AfterSHA) || d.BeforeSHA == d.AfterSHA || !branchChangeSHA.MatchString(d.BeforeTree) || !branchChangeSHA.MatchString(d.AfterTree) || len(d.Files) == 0 || len(d.Files) > 256 {
		return out, errors.New("invalid complete review source identity")
	}
	bytes, lines := 0, 0
	versions := make([][2]ReviewProjectedVersion, 0, len(d.Files))
	last := ""
	for _, f := range d.Files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if len(f.Path) > 4096 || !reviewPath(f.Path) || f.Path <= last || f.Before == f.After {
			return out, errors.New("invalid, duplicate, unordered or unchanged review leaf")
		}
		last = f.Path
		b, err := projectionVersion(f.Before)
		if err != nil {
			return out, err
		}
		a, err := projectionVersion(f.After)
		if err != nil {
			return out, err
		}
		bytes += b.Bytes + a.Bytes
		lines += b.Lines + a.Lines
		if bytes > 4<<20 || lines > reviewProjectionMaxLines {
			return out, errors.New("review projection complete-source work bound exceeded")
		}
		versions = append(versions, [2]ReviewProjectedVersion{b, a})
	}
	out = ReviewProjection{Format: ReviewProjectionFormat, Algorithm: ReviewProjectionAlgorithm, ContextLines: ReviewProjectionContext, SourceDigest: d.Digest(), BeforeSHA: d.BeforeSHA, AfterSHA: d.AfterSHA, BeforeTree: d.BeforeTree, AfterTree: d.AfterTree, Files: make([]ReviewProjectedFile, 0, len(d.Files))}
	estimate := 0
	for i, f := range d.Files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		b, a := versions[i][0], versions[i][1]
		before, after := reviewLines(f.Before.Content), reviewLines(f.After.Content)
		hunks := reviewHunks(reviewEdits(before, after))
		if err := verifyReviewHunks(before, after, hunks); err != nil {
			return out, err
		}
		file := ReviewProjectedFile{Path: f.Path, Before: b, After: a, Hunks: hunks}
		encoded, err := json.Marshal(file)
		if err != nil {
			return out, err
		}
		estimate += len(encoded)
		if estimate > ReviewProjectionMaxBytes {
			return out, errors.New("complete review projection exceeds model-input shape bound")
		}
		out.Files = append(out.Files, file)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if len(encoded) > ReviewProjectionMaxBytes {
		return out, errors.New("complete review projection exceeds model-input shape bound")
	}
	return out, ctx.Err()
}
func (p ReviewProjection) Digest() string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ValidateReviewProjection rederives the exact versioned format; a caller may
// not supply truncated hunks or substitute a projection of another source.
func ValidateReviewProjection(ctx context.Context, d ReviewDiff, p ReviewProjection) error {
	expected, err := ProjectReviewDiff(ctx, d)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, p) {
		return errors.New("review projection changed or is incomplete")
	}
	return nil
}

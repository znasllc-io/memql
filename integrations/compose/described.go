package compose

import (
	"encoding/json"
	"math"
	"path"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/core/num"
)

// described.go -- which described Markdown requests the Library's revision workflow can write.
//
// A Markdown file asked for by DESCRIBING it (describedMarkdown, materialize.go) is filed
// empty, and the person's own client hands the description to the Library's revision
// workflow together with the Library files they picked. That workflow can read only what
// the Library attaches to a review comment: a Markdown file or a picture, each within a size
// limit. A picked file it cannot attach -- a spreadsheet, JSON, text, a web page, an archive,
// a PDF, a file past the limit, one in the Bin, one nobody can read -- would be dropped, and
// "mix files, describe it, get the file" must use every file it is given. So such a request
// goes to the composer instead, which reads them all.
//
// THE RULE IS THE LIBRARY'S, COPIED. integrations/library enforces it
// (review_attachments.go) and this package may not import that one -- the area graph
// forbids the edge -- so the two limits and the picture formats are declared again below.
// TestComposeMirrorsTheReviewAttachmentRules (integrations/library) reads this file and fails
// when they differ. The SDK holds the same rule in TypeScript (attachmentKind, in
// sdk/ts/src/client/materialize.ts) and decides a request's path with it, so the person's
// client hands on exactly the files this server filed empty; that test file holds the SDK's
// copy too. Declare each of the three on a line of its own at the top level, as the Library
// does: that test reads them off the source.
//
// IT JUDGES A FILE ON ITS ROW, NOT ON ITS BYTES. The name and the size are all that is
// known before any content is read, and the path is decided before any content is read
// (materialize). What only the bytes can show -- Markdown that is not UTF-8, a picture that
// does not decode, an extension that lies about the format -- is the Library's to refuse
// when the description is handed on, and the hand-off then leaves that reference off and
// names it (handOffDescription). That is the cost of deciding first, and the smaller one:
// reading first downloaded, and could refuse, files that nothing used.

const libraryAttachMarkdownBytes = 32 << 10                            // the Library's reviewMarkdownBytes
const libraryAttachImageBytes = 8 << 20                                // the Library's reviewImageBytes
var libraryAttachImageFormats = []string{"png", "jpeg", "gif", "webp"} // reviewImageFormats; ".jpg" is read as ".jpeg"

// attachableByLibrary: the Library would take a file of this name and size as a review
// attachment. The extension is path.Ext of the name, lower-cased; ".md" up to
// libraryAttachMarkdownBytes, an image format up to libraryAttachImageBytes. A size of
// 0 is not attachable; sized=false (the row says no size) is not held against it.
func attachableByLibrary(name string, size int64, sized bool) bool {
	ext := strings.ToLower(path.Ext(name))
	if ext == ".jpg" {
		ext = ".jpeg"
	}
	fits := func(limit int64) bool { return !sized || (size > 0 && size <= limit) }
	if ext == ".md" {
		return fits(libraryAttachMarkdownBytes)
	}
	return slices.Contains(libraryAttachImageFormats, strings.TrimPrefix(ext, ".")) && fits(libraryAttachImageBytes)
}

// rowSize: a file row's "size" when it is a JSON number (int, int64, float64, json.Number).
// Anything else, including a row with no size, is no size; the caller does not hold that
// against the file.
func rowSize(row map[string]any) (size int64, sized bool) {
	switch n := row["size"].(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return bytesOfFloat(n)
	case json.Number:
		if whole, err := n.Int64(); err == nil {
			return whole, true
		}
		if fraction, err := n.Float64(); err == nil {
			return bytesOfFloat(fraction)
		}
	}
	return 0, false
}

// bytesOfFloat reads a decoded size. A file's size is an int, so a fraction of a byte does
// not occur; one is read all the same, rounded UP, so that "more than 0 and at most the
// limit" gives the answer for it that the SDK's `size > 0 && size <= limit` gives. A number
// that is not finite is no size, as it is there.
//
// narrowing: SATURATE -- a size past what an int64 holds is past every limit, and
// saturating keeps it there (num.ClampFloat64ToInt64).
func bytesOfFloat(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return num.ClampFloat64ToInt64(math.Ceil(f)), true
}

// attachableSource: a library_file source that resolved to exactly one row, with no
// problem, not archived, whose name and size attachableByLibrary takes.
func attachableSource(r Resolved) bool {
	if r.Ref.Kind != KindLibraryFile || r.Problem != "" || len(r.Rows) != 1 {
		return false
	}
	row := r.Rows[0]
	if boolOf(row["archived"]) {
		return false
	}
	size, sized := rowSize(row)
	return attachableByLibrary(stringOf(row["name"]), size, sized)
}

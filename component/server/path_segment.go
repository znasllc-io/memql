package server

import "strings"

// path_segment.go -- one implementation of "the single path segment between a
// prefix and a suffix" (memql#3773).
//
// WHY THIS IS A FUNCTION AND NOT A THIRD COPY OF THE GUARD. Three handlers in
// this package needed this parse, and all three were written the same way:
//
//	if !strings.HasPrefix(path, prefix) { ... }
//	if !strings.HasSuffix(path, suffix) { ... }
//	middle := path[len(prefix) : len(path)-len(suffix)]
//
// which panics. HasPrefix and HasSuffix can BOTH hold while the regions they
// matched overlap -- true for every path shorter than len(prefix)+len(suffix).
// "/spaces/attachments" starts with "/spaces/" and ends with "/attachments"
// with nothing of its own in between, so the slice is path[8:7]: start past
// end, `slice bounds out of range [8:7]`.
//
// The shape SPREAD. site_bundle_handler.go's parseSiteBundlePublishPath was
// modelled on attachment_handler.go's version and inherited the defect; it got
// a length guard in memql#3713, in that file only, with a comment noting the
// original still had the bug. Nobody checked the third --
// extractAutomationNameFromPath in http_contract.go -- which had it too, on
// POST /automations/trigger, and was found only when this issue was picked up.
//
// Two of three copies were wrong at once, and the one that was right was right
// locally, so adding a fourth guard would leave the next author copying a
// pattern again. A function is the fix: the guard now exists once, and a caller
// gets it by calling rather than by remembering.
//
// Callers keep their own additional rules. parseSiteBundlePublishPath layers
// fs.ValidPath and an explicit "." rejection on top, because its segment
// becomes a blob storage key and path traversal is a different concern from
// slice arithmetic.

// segmentBetween returns the single path segment between prefix and suffix.
//
// ok is false unless the path starts with prefix, ends with suffix, the two do
// not overlap, and what lies between them is exactly one non-empty segment
// (whitespace-trimmed, containing no "/").
//
// The length check is `<` rather than `<=`: a path exactly len(prefix)+
// len(suffix) long -- "/spaces//attachments" -- slices legally to the empty
// string, and is refused a line later for BEING empty. Keeping those two
// rejections separate is deliberate; collapsing them into `<=` would return the
// right answer for the wrong reason and blur what the guard is for.
func segmentBetween(path, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	if len(path) < len(prefix)+len(suffix) {
		return "", false
	}
	segment := strings.TrimSpace(path[len(prefix) : len(path)-len(suffix)])
	if segment == "" || strings.Contains(segment, "/") {
		return "", false
	}
	return segment, true
}

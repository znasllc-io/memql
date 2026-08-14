package server

import (
	"testing"
)

// path_segment_test.go -- memql#3773.
//
// THE DEFECT. Three handlers in this package parse one path segment out of
// {prefix}{segment}{suffix} by slicing:
//
//	middle := path[len(prefix) : len(path)-len(suffix)]
//
// guarded only by HasPrefix and HasSuffix. Those two can BOTH be true while the
// regions they matched OVERLAP -- which happens for any path shorter than
// len(prefix)+len(suffix). "/spaces/attachments" starts with "/spaces/" and ends
// with "/attachments" with nothing of its own in between, so the slice becomes
// path[8:7]: start greater than end, and Go panics with
//
//	runtime error: slice bounds out of range [8:7]
//
// WHY THERE ARE THREE. The shape was COPIED. site_bundle_handler.go's
// parseSiteBundlePublishPath says so in its own comment -- it was modelled on
// extractPartitionIdFromAttachmentPath, inherited the bug, and was fixed in
// memql#3713 by adding a length guard in that one file while leaving the
// original alone (filed as this issue). Nobody looked at the third,
// extractAutomationNameFromPath in http_contract.go, which has the identical
// unguarded slice and panics on POST /automations/trigger.
//
// So the fix is not a third length guard. It is ONE helper the three call, so
// that the next handler needing this shape copies a function instead of a
// pattern. This file tests the helper directly and pins the panicking input for
// each of the three call sites, because a guard that exists in one file and not
// its two siblings is exactly the state that produced the bug twice.
//
// The blast radius was bounded and this is not a crash: net/http recovers a
// panicking handler per connection, so the effect is a dropped connection with
// no HTTP response rather than a downed node. It is still a panic reachable by
// an unauthenticated request shape, and it returns no 400 where one is owed.

func TestSegmentBetween(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		prefix, suffix string
		want           string
		wantOK         bool
	}{
		// The regression. Each of these is the exact input that panicked the
		// corresponding handler before this change.
		{
			name:   "overlap: attachment path with no partition id (memql#3773)",
			path:   "/spaces/attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		{
			name:   "overlap: automation path with no name",
			path:   "/automations/trigger",
			prefix: "/automations/", suffix: "/trigger",
			want: "", wantOK: false,
		},
		{
			name:   "overlap: site bundle path with no site id",
			path:   "/sites/bundles",
			prefix: "/sites/", suffix: "/bundles",
			want: "", wantOK: false,
		},
		// Shorter than the prefix alone -- HasPrefix fails first, but the
		// arithmetic would be even further out of range if it did not.
		{
			name: "shorter than the prefix", path: "/spa",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		// Exactly prefix+suffix with nothing between: the boundary case, one
		// character away from the overlap above. len(path) == len(prefix)+
		// len(suffix) makes the slice legal but empty, so this must be rejected
		// by the empty check rather than by the length guard -- a guard written
		// as `<=` would reject it for the wrong reason and a guard written as
		// `<` must leave it to the emptiness check. Pinning it keeps a later
		// simplification from silently swapping which check fires.
		{
			name: "prefix and suffix adjacent, empty segment", path: "/spaces//attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		// Ordinary success.
		{
			name: "single segment", path: "/spaces/v1:cognition:space:abc/attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "v1:cognition:space:abc", wantOK: true,
		},
		{
			name: "segment is trimmed", path: "/spaces/  abc  /attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "abc", wantOK: true,
		},
		// More than one segment is not a single segment.
		{
			name: "multiple segments rejected", path: "/spaces/a/b/attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		// Whitespace-only collapses to empty after trimming.
		{
			name: "whitespace-only segment", path: "/spaces/   /attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		{
			name: "no prefix match", path: "/other/abc/attachments",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		{
			name: "no suffix match", path: "/spaces/abc/other",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
		{
			name: "empty path", path: "",
			prefix: "/spaces/", suffix: "/attachments",
			want: "", wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := segmentBetween(tt.path, tt.prefix, tt.suffix)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("segmentBetween(%q, %q, %q) = (%q, %v), want (%q, %v)",
					tt.path, tt.prefix, tt.suffix, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestPathParsersSurviveTheOverlapCase pins the bug at each of the three CALL
// SITES, not only in the shared helper.
//
// The helper being correct is not the property that matters to a caller: the
// defect was a call site doing the arithmetic itself, and a later edit could
// reintroduce it in any one of these three without touching the helper at all.
// Each subtest passes the exact request path that panicked, and a panic here
// fails the test rather than the process (go test recovers per-test), so this
// reports as a normal failure naming the function.
func TestPathParsersSurviveTheOverlapCase(t *testing.T) {
	t.Run("extractPartitionIdFromAttachmentPath", func(t *testing.T) {
		if got := extractPartitionIdFromAttachmentPath("/spaces/attachments"); got != "" {
			t.Errorf("got %q, want \"\" -- a path with no partition id must not parse", got)
		}
	})

	t.Run("extractAutomationNameFromPath", func(t *testing.T) {
		got := extractAutomationNameFromPath("/automations/trigger", "/automations/", "/trigger")
		if got != "" {
			t.Errorf("got %q, want \"\" -- a path with no automation name must not parse", got)
		}
	})

	t.Run("parseSiteBundlePublishPath", func(t *testing.T) {
		if got, ok := parseSiteBundlePublishPath("/sites/bundles"); ok || got != "" {
			t.Errorf("got (%q, %v), want (\"\", false) -- a path with no site id must not parse", got, ok)
		}
	})
}

// TestParseAttachmentDownloadPathHandlesShortPaths is the "also worth checking"
// the issue asked for.
//
// parseAttachmentDownloadPath parses the ADJACENT route in the same file by a
// different method -- split on "/", then index the neighbours of the
// "attachments" segment -- so it does not share the slice arithmetic and cannot
// have this defect. But "it uses a different approach" is an argument, not a
// measurement, and the whole reason this bug reached three functions is that
// each was judged by reading rather than by running. These are the inputs that
// would break an index-based parser: the segment at either end of the list,
// alone, and absent.
func TestParseAttachmentDownloadPathHandlesShortPaths(t *testing.T) {
	for _, path := range []string{
		"",
		"/",
		"attachments",
		"/attachments",
		"/attachments/",
		"//attachments//",
		"/attachments/abc", // no partition id before it
		"/spaces/abc/attachments",
		"/spaces//attachments/abc",
		"/spaces/abc/attachments/",
	} {
		t.Run(path, func(t *testing.T) {
			partitionId, attachmentId, ok := parseAttachmentDownloadPath(path)
			if ok {
				t.Errorf("parseAttachmentDownloadPath(%q) = (%q, %q, true), want ok=false",
					path, partitionId, attachmentId)
			}
		})
	}

	// The positive case, so the negatives above are not passing because the
	// function rejects everything.
	partitionId, attachmentId, ok := parseAttachmentDownloadPath("/spaces/space-1/attachments/att-1")
	if !ok || partitionId != "space-1" || attachmentId != "att-1" {
		t.Errorf("parseAttachmentDownloadPath(valid) = (%q, %q, %v), want (\"space-1\", \"att-1\", true)",
			partitionId, attachmentId, ok)
	}
}

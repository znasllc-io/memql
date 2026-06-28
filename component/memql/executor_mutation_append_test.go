package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests pin the memql#2240 @appendFields engine primitive:
// appendPayloadFields rewrites a partial update payload so the named
// array fields' elements are APPENDED to the stored (prior) array rather
// than replacing it wholesale. This is what lets attachToRequest be a
// pure single-writer mutation (no read-merge-append logic wrapper) while
// still accumulating attachmentIds without clobbering.
//
// Pure-Go engine path: identical on whichever node runs the update; no
// cross-node state involved.

// First attach: prior has no array yet -> append starts fresh with the
// single new id.
func TestAppendPayloadFields_FirstAttachStartsFresh(t *testing.T) {
	prior := map[string]any{"body": "hi"}
	partial := map[string]any{"attachmentIds": "att-1"}

	appendPayloadFields(prior, partial, []string{"attachmentIds"})
	mergePayloadFields(prior, partial, nil)

	require.Equal(t, []any{"att-1"}, prior["attachmentIds"])
	require.Equal(t, "hi", prior["body"], "non-append fields untouched")
}

// Subsequent attach: the new id is appended after the existing ids; order
// preserved, nothing clobbered.
func TestAppendPayloadFields_AppendsWithoutClobber(t *testing.T) {
	prior := map[string]any{"attachmentIds": []any{"att-1", "att-2"}}
	partial := map[string]any{"attachmentIds": "att-3"}

	appendPayloadFields(prior, partial, []string{"attachmentIds"})
	mergePayloadFields(prior, partial, nil)

	require.Equal(t, []any{"att-1", "att-2", "att-3"}, prior["attachmentIds"])
}

// A []string prior (as it may arrive from storage) is handled too.
func TestAppendPayloadFields_PriorStringSlice(t *testing.T) {
	prior := map[string]any{"attachmentIds": []string{"att-1"}}
	partial := map[string]any{"attachmentIds": "att-2"}

	appendPayloadFields(prior, partial, []string{"attachmentIds"})
	mergePayloadFields(prior, partial, nil)

	require.Equal(t, []any{"att-1", "att-2"}, prior["attachmentIds"])
}

// A field NOT written in this call is left untouched (the stored array
// survives via the omitted-field inherit), even when listed in appendFields.
func TestAppendPayloadFields_OmittedFieldUntouched(t *testing.T) {
	prior := map[string]any{"attachmentIds": []any{"att-1"}}
	partial := map[string]any{"body": "edit"}

	appendPayloadFields(prior, partial, []string{"attachmentIds"})
	mergePayloadFields(prior, partial, nil)

	require.Equal(t, []any{"att-1"}, prior["attachmentIds"], "omitted append field must not be wiped")
	require.Equal(t, "edit", prior["body"])
}

// Without @appendFields (no append list) the default top-level-replace
// contract still applies -- a partial array replaces the stored array.
func TestAppendPayloadFields_DefaultReplaceWhenNotListed(t *testing.T) {
	prior := map[string]any{"attachmentIds": []any{"att-1", "att-2"}}
	partial := map[string]any{"attachmentIds": []any{"att-9"}}

	appendPayloadFields(prior, partial, nil) // not opted in
	mergePayloadFields(prior, partial, nil)

	require.Equal(t, []any{"att-9"}, prior["attachmentIds"], "without @appendFields, array replaces wholesale")
}

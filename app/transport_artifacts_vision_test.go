//go:build bff

package app

import (
	"strings"
	"testing"
)

// The Library's byte routes (newLibraryAnalyzer): the extractor an upload is
// read with.
func TestTheLibrarysExtractorResolvesAVisionModelForTheUploader(t *testing.T) {
	router := &visionResolver{vision: &recordingVision{}}
	a := appResolvingVisionThrough(router)

	extractor := a.libraryExtractor()
	if len(router.resolvedFor) != 0 {
		t.Fatalf("building the extractor resolved a model for %q: nothing had been uploaded yet", router.resolvedFor)
	}
	text, err := extractor.Extract(asPerson("ana"), "image/png", []byte{1})
	if err != nil || text != "a picture of a harbour" {
		t.Fatalf("ana's upload: %q, %v", text, err)
	}
	if got := strings.Join(router.resolvedFor, ","); got != "ana" {
		t.Fatalf("the router was asked for %q, want ana: the upload is hers", got)
	}
	// A format that needs no model asks for none.
	if _, err := extractor.Extract(asPerson("ben"), "text/plain", []byte("notes")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(router.resolvedFor, ","); got != "ana" {
		t.Fatalf("reading plain text asked the router for a vision model: %q", got)
	}
}

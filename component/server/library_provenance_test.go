package server

import (
	"reflect"
	"strings"
	"testing"
)

// producedBy is ABSENT on anything a PERSON uploaded (epic memql#5391, design
// D9's second half), and this is the structural half of that guarantee: the
// upload route physically cannot set it, because the params struct it hands
// createLibraryFile has no field for it.
//
// THE ABSENCE IS THE ANSWER. A stamp naming no app would read as "an app
// produced this and reported nothing", which is a different fact from "nobody
// delegated this" -- and the second is what every browser upload is. Folding
// them would make the Files app unable to say which of somebody's files were
// made for them, which is the question the field exists to answer.
//
// A stamp reaches an artifact through exactly one route: the SESSION RUNNER at
// end, through @serverOnly mutations, under the owner's actor with internal
// origin. This test is what stops a second route appearing here by accident --
// the natural way it would happen is somebody adding a ProducedBy to these
// params "for completeness" and wiring it to a request field.
func TestTheUploadRouteCannotStampProvenance(t *testing.T) {
	for _, tc := range []struct {
		what string
		typ  reflect.Type
	}{
		{"LibraryFileCreateParams", reflect.TypeOf(LibraryFileCreateParams{})},
	} {
		for i := 0; i < tc.typ.NumField(); i++ {
			name := tc.typ.Field(i).Name
			if strings.EqualFold(name, "producedBy") {
				t.Fatalf("%s carries a %s field: an upload route that can stamp provenance is a "+
					"primitive for claiming an app produced a file a person uploaded, and a lifted "+
					"construct's source reads that stamp", tc.what, name)
			}
		}
	}
}

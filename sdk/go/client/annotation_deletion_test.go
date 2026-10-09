package client

import "testing"

func TestAnnotationDeletionIncludesBothIDs(t *testing.T) {
	call := LibraryRemoveDocumentAnnotationBuild(LibraryRemoveDocumentAnnotationArgs{ArtifactId: "document-id", CommentId: "annotation-id"})
	const want = `builtin libraryRemoveDocumentAnnotation(artifactId: "document-id", commentId: "annotation-id")`
	if call != want {
		t.Fatalf("deletion call = %q, want %q", call, want)
	}
}

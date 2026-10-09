package server

import (
	"encoding/json"
	"testing"
)

func TestArtifactUploadResponseUsesBareWireIDs(t *testing.T) {
	for _, response := range []ArtifactUploadResponse{
		{ArtifactId: "v1:library:artifact:artifact-hash", FileId: "v1:library:file:file-id", VersionNumber: 2},
		{ArtifactId: "artifact-hash", FileId: "file-id", VersionNumber: 2},
	} {
		original := response
		body, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var got ArtifactUploadResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.ArtifactId != "artifact-hash" || got.FileId != "file-id" || got.VersionNumber != 2 {
			t.Fatalf("upload response is not usable by a bare-ID client: %s", body)
		}
		if response != original {
			t.Fatal("serialization mutated internal identity")
		}
	}
}

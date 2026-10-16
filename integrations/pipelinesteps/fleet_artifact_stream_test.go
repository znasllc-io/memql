//go:build agent

package pipelinesteps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func artifactData(data []byte) *nodev1.WorkerForwardStream {
	return &nodev1.WorkerForwardStream{Payload: &nodev1.WorkerForwardStream_DataChunk{DataChunk: data}}
}

func TestFleetArtifactStreamExceedsOneMessageWithoutBufferingIt(t *testing.T) {
	stream, err := newFleetArtifactStream(filepath.Join(t.TempDir(), "archive"), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	chunk := bytes.Repeat([]byte{0xf3}, fleetArtifactChunkBytes)
	want := sha256.New()
	for i := 0; i < 100; i++ {
		chunk[0] = byte(i)
		stream.chunk(artifactData(chunk))
		want.Write(chunk)
	}
	stream.seal()
	// A late callback cannot mutate a terminal artifact or race its removal.
	stream.chunk(artifactData([]byte("late")))
	file, err := stream.verified(100*fleetArtifactChunkBytes, fmt.Sprintf("sha256:%x", want.Sum(nil)))
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private archive: %v, %v", info, err)
	}
	got := sha256.New()
	if _, err := io.Copy(got, file); err != nil || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatalf("retained bytes changed: %v", err)
	}
}

func TestFleetArtifactStreamRejectsIncompleteCorruptAndOversizedDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   [][]byte
		limit  int64
		size   int64
		digest string
		seal   bool
	}{
		{name: "not terminal", data: [][]byte{[]byte("ok")}, limit: 9, size: 2, digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("ok")))},
		{name: "missing", limit: 9, size: 2, seal: true},
		{name: "truncated", data: [][]byte{[]byte("o")}, limit: 9, size: 2, seal: true},
		{name: "changed", data: [][]byte{[]byte("no")}, limit: 9, size: 2, digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("ok"))), seal: true},
		{name: "duplicated", data: [][]byte{[]byte("ok"), []byte("ok")}, limit: 9, size: 2, digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("ok"))), seal: true},
		{name: "over quota", data: [][]byte{[]byte("1234567890")}, limit: 9, size: 10, seal: true},
		{name: "oversized frame", data: [][]byte{make([]byte, fleetArtifactChunkBytes+1)}, limit: 1 << 20, seal: true},
		{name: "empty frame", data: [][]byte{{}}, limit: 9, seal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := newFleetArtifactStream(filepath.Join(t.TempDir(), "archive"), tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.close()
			for _, chunk := range tc.data {
				stream.chunk(artifactData(chunk))
			}
			if tc.seal {
				stream.seal()
			}
			if _, err := stream.verified(tc.size, tc.digest); err == nil {
				t.Fatal("unverified archive accepted")
			}
			info, err := stream.file.Stat()
			if err != nil || info.Size() > tc.limit {
				t.Fatalf("spool exceeded its quota: %v, %v", info, err)
			}
		})
	}
}

package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Real incompressible bytes exceed BOTH old limits: 64 MiB raw and 20 MiB
// inline. The callback retains frame references, as an asynchronous transport
// may do, so reusing a mutable send buffer would corrupt the returned digest.
func TestPipelineArtifactStreamExceedsFormerLimits(t *testing.T) {
	dir := t.TempDir()
	source, err := os.Create(filepath.Join(dir, "binary"))
	if err != nil {
		t.Fatal(err)
	}
	original := sha256.New()
	const rawBytes = 65 << 20
	if _, err := io.CopyN(io.MultiWriter(source, original), rand.Reader, rawBytes); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	pack := packArtifacts(context.Background(), dir, []string{"."})
	if pack.err != nil || pack.tooLarge || len(pack.missing) != 0 || pack.path == "" {
		t.Fatalf("pack: %+v", pack)
	}
	defer os.Remove(pack.path)
	info, err := os.Stat(pack.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || info.Size() <= 20<<20 {
		t.Fatalf("archive: %v", info)
	}
	var frames [][]byte
	size, digest, err := streamPipelineArtifact(context.Background(), pack.path, func(frame []byte) error {
		if len(frame) == 0 || len(frame) > 256<<10 {
			t.Fatalf("frame size: %d", len(frame))
		}
		frames = append(frames, frame)
		return nil
	})
	if err != nil || size != info.Size() {
		t.Fatalf("stream: %d %s %v", size, digest, err)
	}
	returned := sha256.New()
	for _, frame := range frames {
		returned.Write(frame)
	}
	if digest != fmt.Sprintf("sha256:%x", returned.Sum(nil)) {
		t.Fatal("frames mutated after send or terminal digest mismatch")
	}
	archive, err := os.Open(pack.path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil || header.Name != "binary" || header.Size != rawBytes {
		t.Fatalf("header: %+v %v", header, err)
	}
	actual := sha256.New()
	if n, err := io.Copy(actual, tr); n != rawBytes || err != nil {
		t.Fatalf("contents: %d %v", n, err)
	}
	if fmt.Sprintf("%x", actual.Sum(nil)) != fmt.Sprintf("%x", original.Sum(nil)) {
		t.Fatal("artifact bytes changed")
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("archive packed itself: %v", err)
	}
}

func TestPipelineArtifactStreamStopsOnLostTransportAndCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(path, make([]byte, 3*256<<10), 0600); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("stream disconnected")
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			size, digest, err := streamPipelineArtifact(ctx, path, func([]byte) error {
				calls++
				if cancelled {
					cancel()
					return nil
				}
				return lost
			})
			want := lost
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || calls != 1 || size != 0 || digest != "" {
				t.Fatalf("false receipt: %d %s %v calls=%d", size, digest, err, calls)
			}
		})
	}
	if _, _, err := streamPipelineArtifact(context.Background(), path, nil); err == nil {
		t.Fatal("missing transport accepted")
	}
}

func TestPipelineArtifactInventoryIsBoundedAndCancellable(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 1025; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pack := packArtifacts(context.Background(), dir, []string{"."})
	if !pack.tooLarge || pack.path != "" {
		t.Fatalf("unbounded inventory: %+v", pack)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pack = packArtifacts(ctx, dir, []string{"."})
	if !errors.Is(pack.err, context.Canceled) || pack.path != "" {
		t.Fatalf("ignored cancellation: %+v", pack)
	}
}

func TestPipelineArtifactDispatchFailureCleansWorkspace(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, root := pipelineTestPolicy(t, "")
	d := NewDispatcher(quietLogger(), p, nil)
	success, fail := d.DispatchStream(context.Background(), pipelineDispatch(t, "artifact-call", stepArgs(t, fx, "echo proof > proof.txt", map[string]any{"artifacts": []any{"proof.txt"}})), func(frame *memqlv1.ToolStream) error {
		if frame.GetCallId() != "artifact-call" {
			t.Fatalf("wrong call: %q", frame.GetCallId())
		}
		if len(frame.GetDataChunk()) > 0 {
			return errors.New("lost transport")
		}
		return nil
	})
	if success != nil || fail == nil || fail.GetErrorCode() != "artifact_failed" {
		t.Fatalf("false success: %+v %+v", success, fail)
	}
	assertNoStepDirs(t, root)
	// A completed cleanup releases the durable shared reservation for another step.
	if res := decoded(t)(runPipelineStepQuietly(t, stepArgs(t, fx, "true", nil), p)); res.ExitCode != 0 {
		t.Fatal(res)
	}
}

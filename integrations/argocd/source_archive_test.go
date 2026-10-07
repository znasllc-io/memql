package argocd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sourceArchiveFixture(t *testing.T) ([]byte, RenderSpec, ClosedSource) {
	t.Helper()
	objects, spec := sourceObjectsFixture(t, sourceFilesFixture())
	var archive bytes.Buffer
	closed, err := CaptureSourceArchive(context.Background(), objects, spec, &archive)
	require.NoError(t, err)
	return archive.Bytes(), spec, closed
}

func TestSourceArchiveReverifiesIndependentCapturedObjects(t *testing.T) {
	body, spec, captured := sourceArchiveFixture(t)
	verified, err := VerifySourceArchive(context.Background(), body, spec)
	require.NoError(t, err)
	require.Equal(t, captured.Digest(), verified.Digest())
	require.Equal(t, captured.Files(), verified.Files())
	// A new capture has no dependence on a previous verifier's maps or order.
	again, _, _ := sourceArchiveFixture(t)
	require.Equal(t, body, again)
	for i := range body {
		body[i] = 'x'
	}
	require.Equal(t, captured.Digest(), verified.Digest())
	require.Equal(t, captured.Files(), verified.Files())
}

func TestSourceArchiveRequiresEndRecordsBeyondZeroFilledContent(t *testing.T) {
	for _, size := range []int{1024, 1025} {
		objects, spec := sourceObjectsFixture(t, map[string]sourceFixtureFile{
			"overlay/kustomization.yaml": {"100644", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nconfigMapGenerator:\n  - name: binary\n    files: [payload.bin]\n"},
			"overlay/payload.bin":        {"100644", string(make([]byte, size))},
		})
		var archive bytes.Buffer
		_, err := CaptureSourceArchive(context.Background(), objects, spec, &archive)
		require.NoError(t, err)
		_, err = VerifySourceArchive(context.Background(), archive.Bytes(), spec)
		require.NoError(t, err)
		for _, removed := range []int{512, 1024} {
			truncated := archive.Bytes()[:archive.Len()-removed]
			require.True(t, allZero(truncated[len(truncated)-1024:]))
			closed, err := VerifySourceArchive(context.Background(), truncated, spec)
			require.Error(t, err, "blob size %d, removed %d bytes: content and padding cannot stand in for end records", size, removed)
			require.Empty(t, closed.Digest())
		}
	}
}

func TestSourceArchiveRejectsDuplicateObjectsAndDifferentSource(t *testing.T) {
	body, spec, _ := sourceArchiveFixture(t)
	r := tar.NewReader(bytes.NewReader(body))
	first, err := r.Next()
	require.NoError(t, err)
	entryBytes := 512 + (int(first.Size)+511)/512*512
	duplicate := append(append([]byte(nil), body[:entryBytes]...), body...)
	_, err = VerifySourceArchive(context.Background(), duplicate, spec)
	require.ErrorContains(t, err, "duplicate")
	for _, field := range []string{"path", "targetRevision"} {
		var source map[string]any
		require.NoError(t, json.Unmarshal(spec.Source, &source))
		if field == "path" {
			source[field] = "another-overlay"
		} else {
			source[field] = strings.Repeat("f", 40)
		}
		other := spec
		other.Source, err = json.Marshal(source)
		require.NoError(t, err)
		closed, err := VerifySourceArchive(context.Background(), body, other)
		require.Error(t, err)
		require.Empty(t, closed.Digest())
	}
}

func rewriteSourceArchive(t *testing.T, body []byte, change func(*tar.Header, []byte) (*tar.Header, []byte), extra bool) []byte {
	t.Helper()
	var output bytes.Buffer
	w := tar.NewWriter(&output)
	r := tar.NewReader(bytes.NewReader(body))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(r)
		require.NoError(t, err)
		h, data = change(h, data)
		if h == nil {
			continue
		}
		require.NoError(t, w.WriteHeader(h))
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	if extra {
		objects := objectFixture{}
		data := []byte("unrelated private material")
		oid := objects.add("blob", data)
		require.NoError(t, w.WriteHeader(&tar.Header{Name: "blob/" + oid, Mode: 0400, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0)}))
		_, err := w.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return output.Bytes()
}

func TestSourceArchiveRejectsSubstitutionAndArchiveTricks(t *testing.T) {
	original, spec, _ := sourceArchiveFixture(t)
	for name, change := range map[string]func(*tar.Header, []byte) (*tar.Header, []byte){
		"substituted body": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if len(b) > 0 {
				b[0] ^= 1
			}
			return h, b
		},
		"missing commit": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if strings.HasPrefix(h.Name, "commit/") {
				return nil, nil
			}
			return h, b
		},
		"path traversal": func(h *tar.Header, b []byte) (*tar.Header, []byte) { h.Name = "../" + h.Name; return h, b },
		"symlink": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			h.Typeflag = tar.TypeSymlink
			h.Linkname = "/private"
			h.Size = 0
			return h, nil
		},
		"hardlink": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			h.Typeflag = tar.TypeLink
			h.Linkname = "outside"
			h.Size = 0
			return h, nil
		},
		"PAX override": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			h.Format = tar.FormatPAX
			h.PAXRecords = map[string]string{"comment": "unqualified"}
			return h, b
		},
		"object kind": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			h.Name = "tag/" + strings.Repeat("a", 40)
			return h, b
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := rewriteSourceArchive(t, original, change, false)
			closed, err := VerifySourceArchive(context.Background(), body, spec)
			require.Error(t, err)
			require.Empty(t, closed.Digest())
		})
	}
	unchanged := func(h *tar.Header, b []byte) (*tar.Header, []byte) { return h, b }
	extra := rewriteSourceArchive(t, original, unchanged, true)
	_, err := VerifySourceArchive(context.Background(), extra, spec)
	require.ErrorContains(t, err, "outside the verified input closure")
	for name, body := range map[string][]byte{
		"missing end record":   original[:len(original)-512],
		"partial record":       original[:len(original)-1],
		"concatenated archive": append(append([]byte(nil), original...), original...),
		"extra end record":     append(append([]byte(nil), original...), make([]byte, 512)...),
		"oversized":            make([]byte, MaxSourceArchiveBytes+512),
	} {
		t.Run(name, func(t *testing.T) {
			closed, err := VerifySourceArchive(context.Background(), body, spec)
			require.Error(t, err)
			require.Empty(t, closed.Digest())
		})
	}
}

type sourceFailingWriter struct{ remaining int }

func (w *sourceFailingWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("private writer diagnostic")
	}
	w.remaining -= len(p)
	return len(p), nil
}

func TestSourceArchiveRefusesPartialOutputAndCancellation(t *testing.T) {
	body, spec, _ := sourceArchiveFixture(t)
	for _, remaining := range []int{0, len(body) - 512} {
		objects, _ := sourceObjectsFixture(t, sourceFilesFixture())
		closed, err := CaptureSourceArchive(context.Background(), objects, spec, &sourceFailingWriter{remaining: remaining})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private writer diagnostic")
		require.Empty(t, closed.Digest())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closed, err := VerifySourceArchive(ctx, body, spec)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, closed.Digest())
}

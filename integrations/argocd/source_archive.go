package argocd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// MaxSourceArchiveBytes includes one header and at most one padding block per
// bounded Git object, plus the two end records. The format is uncompressed
// USTAR, avoiding an additional decompression or file-extraction trust boundary.
const MaxSourceArchiveBytes = maxSourceBytes + maxSourceCount*1024 + 1024

// CaptureSourceArchive writes only the verified objects actually consumed by
// source closure. The trusted Workbench collector supplies a pinned checkout's
// raw object reader. Its archive is a private artifact, not a public release
// asset: committed inputs and commit messages can contain confidential data.
// On any error the caller MUST discard the partial output. The result is valid
// only after the archive's final records have been written successfully.
func CaptureSourceArchive(ctx context.Context, objects GitObjects, spec RenderSpec, output io.Writer) (ClosedSource, error) {
	if output == nil {
		return ClosedSource{}, errors.New("Git source archive output is unavailable")
	}
	writer := tar.NewWriter(output)
	closed, err := verifySourceClosure(ctx, objects, spec, func(kind, oid string, body []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		header := &tar.Header{
			Name: kind + "/" + oid, Mode: 0400, Size: int64(len(body)),
			Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0),
		}
		if writer.WriteHeader(header) != nil {
			return errors.New("Git source archive could not be written")
		}
		if _, err := writer.Write(body); err != nil {
			return errors.New("Git source archive could not be written")
		}
		return nil
	})
	if err != nil {
		return ClosedSource{}, err
	}
	if err := ctx.Err(); err != nil {
		return ClosedSource{}, err
	}
	if writer.Close() != nil {
		return ClosedSource{}, errors.New("Git source archive did not finish")
	}
	return closed, nil
}

type archivedGitObjects struct {
	objects map[string][]byte
	used    map[string]bool
}

func (a *archivedGitObjects) OpenGitObject(ctx context.Context, kind, oid string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := kind + "/" + oid
	body, found := a.objects[key]
	if !found {
		return nil, errors.New("Git source archive lacks an object")
	}
	a.used[key] = true
	return io.NopCloser(bytes.NewReader(body)), nil
}

// VerifySourceArchive is the engine-side read of an authenticated, bounded
// Workbench artifact. It trusts neither its label nor its collector's claimed
// verdict: the full commit/tree/blob proof and input closure run again against
// the caller's native render specification. It writes nothing to the filesystem
// and returns only owned, content-free evidence. Artifact ownership/provenance
// and permission to prepare an installation remain separate native gates.
func VerifySourceArchive(ctx context.Context, archive []byte, spec RenderSpec) (ClosedSource, error) {
	if len(archive) < 1024 || len(archive) > MaxSourceArchiveBytes || len(archive)%512 != 0 || !allZero(archive[len(archive)-1024:]) {
		return ClosedSource{}, errors.New("Git source archive has an invalid size or terminator")
	}
	input := bytes.NewReader(archive)
	reader := tar.NewReader(input)
	objects := &archivedGitObjects{objects: map[string][]byte{}, used: map[string]bool{}}
	total, padding := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return ClosedSource{}, err
		}
		remaining := input.Len()
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// archive/tar also accepts physical EOF and a single zero record.
			// Count records AFTER the preceding body, so a zero-filled blob
			// cannot impersonate either of our two required end records.
			if remaining-input.Len() != padding+1024 {
				return ClosedSource{}, errors.New("Git source archive lacks complete end records")
			}
			break
		}
		if err != nil {
			return ClosedSource{}, errors.New("Git source archive is malformed")
		}
		kind, oid, _ := strings.Cut(header.Name, "/")
		if (kind != "commit" && kind != "tree" && kind != "blob") || !commitSHA.MatchString(oid) ||
			header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Linkname != "" ||
			header.Mode != 0400 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" ||
			len(header.PAXRecords) != 0 || header.Size < 0 || header.Size > maxSourceObject || header.Size > int64(maxSourceBytes-total) {
			return ClosedSource{}, errors.New("Git source archive has an unsupported entry or exceeds its byte bound")
		}
		if _, duplicate := objects.objects[header.Name]; duplicate || len(objects.objects) >= maxSourceCount {
			return ClosedSource{}, errors.New("Git source archive has duplicate objects or exceeds its object bound")
		}
		body, err := io.ReadAll(reader)
		if err != nil || int64(len(body)) != header.Size {
			return ClosedSource{}, errors.New("Git source archive entry did not complete")
		}
		total += len(body)
		padding = (512 - len(body)%512) % 512
		objects.objects[header.Name] = body
	}
	if input.Len() != 0 {
		return ClosedSource{}, errors.New("Git source archive contains trailing records")
	}
	closed, err := VerifySourceClosure(ctx, objects, spec)
	if err != nil {
		return ClosedSource{}, err
	}
	if len(objects.used) != len(objects.objects) {
		return ClosedSource{}, errors.New("Git source archive contains objects outside the verified input closure")
	}
	return closed, nil
}

func allZero(body []byte) bool {
	for _, b := range body {
		if b != 0 {
			return false
		}
	}
	return true
}

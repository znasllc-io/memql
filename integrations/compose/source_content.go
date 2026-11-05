package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"path"
	"strings"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/core/librarykey"
)

const (
	maxReferenceBytes   = 16 << 20
	maxReferenceText    = 256 << 10
	maxReferenceImages  = 8
	maxReferenceEntries = 64
	maxReferencePixels  = 24_000_000
)

// SourceDownloader resolves storage objects through the installed blob client;
// reference files cannot introduce arbitrary HTTP downloads or redirects.
type SourceDownloader interface {
	DownloadURLWithLimit(context.Context, string, int64) ([]byte, error)
}

// SourceContent is captured in the owner-only execution input before dispatch.
// Another replica composes from the same bytes even if the source changes later.
type SourceContent struct {
	Name          string `json:"name"`
	MimeType      string `json:"mimeType"`
	Text          string `json:"text,omitempty"`
	Image         []byte `json:"image,omitempty"`
	SHA256        string `json:"sha256"`
	EmailAssetURL string `json:"emailAssetUrl,omitempty"`
}

type referenceBudget struct{ bytes, text, images, entries int }

func (i *Integration) captureSourceContents(ctx context.Context, sources []Resolved) error {
	budget := &referenceBudget{}
	assetBytes := 0
	assets := map[string]bool{}
	for n := range sources {
		source := &sources[n]
		if !source.Ref.Content {
			continue
		}
		if len(source.Rows) != 1 || source.Problem != "" {
			return fmt.Errorf("compose: reference %q is not readable by you", source.Ref.Label)
		}
		reader := i.sourceDownloader()
		if reader == nil {
			return errors.New("compose: reading reference files is not configured on this node")
		}
		row := source.Rows[0]
		blob := stringOf(row["blobUrl"])
		if blob == "" {
			return fmt.Errorf("compose: reference %q has no stored content", source.Ref.Label)
		}
		// The row is the caller's; the key must be its owner's too, or a row
		// naming somebody else's object would compose their bytes.
		if err := librarykey.Check(blob, stringOf(row["ownerUserId"]), ""); err != nil {
			return fmt.Errorf("compose: reference %q is not stored in its owner's Library", source.Ref.Label)
		}
		data, err := reader.DownloadURLWithLimit(ctx, blob, maxReferenceBytes)
		if err != nil {
			return fmt.Errorf("compose: reading reference %q: %w", source.Ref.Label, err)
		}
		if len(data) > maxReferenceBytes {
			return errors.New("compose: a reference exceeds the 16 MiB input limit")
		}
		if digest := stringOf(row["sha256"]); digest != "" && !strings.EqualFold(digest, (pure.Result{Bytes: data}).SHA256()) {
			return fmt.Errorf("compose: reference %q no longer matches its recorded content digest", source.Ref.Label)
		}
		files, err := readReference(stringOf(row["name"]), stringOf(row["mimeType"]), data, budget)
		if err != nil {
			return fmt.Errorf("compose: reference %q: %w", source.Ref.Label, err)
		}
		source.Files = files
		if source.Ref.IncludeImages {
			hasImage := false
			for _, file := range files {
				hasImage = hasImage || len(file.Image) > 0
				if len(file.Image) == 0 || assets[file.SHA256] {
					continue
				}
				if err := pure.EmailImageData(file.Image, file.MimeType); err != nil {
					return err
				}
				assets[file.SHA256] = true
				assetBytes += len(file.Image)
				if assetBytes > 1<<20 {
					return errors.New("images selected for inclusion exceed 1 MiB; use smaller logo or photo files and keep full layouts as references")
				}
			}
			if !hasImage {
				return fmt.Errorf("reference %q contains no supported images to include; keep it as a reference only", source.Ref.Label)
			}
		}
	}
	return nil
}

// Archives are inspected only in memory, only through explicit reference input.
// No path is created, no URL is fetched, and no file is executed.
func readReference(name, mime string, data []byte, budget *referenceBudget) ([]SourceContent, error) {
	archive := strings.EqualFold(path.Ext(name), ".zip") || strings.HasPrefix(strings.ToLower(mime), "application/zip") || bytes.HasPrefix(data, []byte{'P', 'K', 3, 4}) || bytes.HasPrefix(data, []byte{'P', 'K', 5, 6})
	if !archive {
		entry, err := readReferenceMember(name, data, budget)
		if err != nil {
			return nil, err
		}
		return []SourceContent{entry}, nil
	}
	limits := MaterializerArchiveLimits
	limits.MaxUnpackedBytes = maxReferenceBytes
	read, err := ReadArchive(data, limits)
	if err != nil {
		return nil, err
	}
	var out []SourceContent
	for _, m := range read.Members {
		entry, err := budget.take(m)
		if err != nil {
			return nil, fmt.Errorf("ZIP member %q: %w", m.Path, err)
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, errors.New("ZIP has no usable references")
	}
	return out, nil
}

func safeReferencePath(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	for _, c := range name {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func readReferenceMember(name string, data []byte, budget *referenceBudget) (SourceContent, error) {
	out := SourceContent{Name: name, SHA256: (pure.Result{Bytes: data}).SHA256()}
	budget.entries++
	budget.bytes += len(data)
	if budget.entries > maxReferenceEntries || budget.bytes > maxReferenceBytes {
		return out, errors.New("references exceed 64 files or 16 MiB expanded")
	}
	member, _, err := classifyMember(name, data, MaterializerArchiveLimits)
	if err != nil {
		return out, err
	}
	return budget.account(member)
}

// take charges one already-validated archive member against the running budget
// shared by every reference of a composition.
func (b *referenceBudget) take(m ArchiveMember) (SourceContent, error) {
	b.entries++
	b.bytes += len(m.Data)
	if b.entries > maxReferenceEntries || b.bytes > maxReferenceBytes {
		return SourceContent{}, errors.New("references exceed 64 files or 16 MiB expanded")
	}
	return b.account(m)
}

func (b *referenceBudget) account(m ArchiveMember) (SourceContent, error) {
	out := SourceContent{Name: m.Path, SHA256: (pure.Result{Bytes: m.Data}).SHA256(), MimeType: m.MIME}
	if strings.HasPrefix(m.MIME, "image/") {
		b.images++
		if b.images > maxReferenceImages {
			return out, imageCountError(MaterializerArchiveLimits)
		}
		out.Image = m.Data
		return out, nil
	}
	b.text += len(m.Data)
	if b.text > maxReferenceText {
		return out, textTotalError(MaterializerArchiveLimits)
	}
	out.Text = string(m.Data)
	return out, nil
}

package compose

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	pure "github.com/znasllc-io/memql/component/compose"
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
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Image    []byte `json:"image,omitempty"`
	SHA256   string `json:"sha256"`
}

type referenceBudget struct{ bytes, text, images, entries int }

func (i *Integration) captureSourceContents(ctx context.Context, sources []Resolved) error {
	budget := &referenceBudget{}
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
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid ZIP: %w", err)
	}
	if len(zr.File) > maxReferenceEntries {
		return nil, errors.New("ZIP contains more than 64 entries")
	}
	seen := map[string]bool{}
	var out []SourceContent
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !safeReferencePath(name) || seen[name] {
			return nil, fmt.Errorf("ZIP has an unsafe or duplicate member path %q", f.Name)
		}
		seen[name] = true
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return nil, fmt.Errorf("ZIP member %q is not a regular file", f.Name)
		}
		// Finder's packaging metadata is not user material. Its contents are never
		// opened, so skipping it cannot evade the expanded-byte budget.
		if strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store" {
			continue
		}
		remaining := maxReferenceBytes - budget.bytes
		if remaining <= 0 || f.UncompressedSize64 > uint64(remaining) {
			return nil, errors.New("reference contents exceed 16 MiB expanded")
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		member, readErr := io.ReadAll(io.LimitReader(r, int64(remaining)+1))
		closeErr := r.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		entry, err := readReferenceMember(name, member, budget)
		if err != nil {
			return nil, fmt.Errorf("ZIP member %q: %w", name, err)
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
	if len(data) == 0 {
		return out, errors.New("reference is empty")
	}
	ext := strings.ToLower(path.Ext(name))
	if bytes.HasPrefix(data, []byte{'P', 'K'}) || ext == ".zip" {
		return out, errors.New("nested archives are not supported; provide their files directly")
	}
	if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" {
		config, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return out, errors.New("image content is invalid")
		}
		if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxReferencePixels {
			return out, errors.New("reference image exceeds 24 million pixels")
		}
		budget.images++
		if budget.images > maxReferenceImages {
			return out, errors.New("use up to eight reference images per composition")
		}
		out.MimeType = "image/" + format
		out.Image = data
		return out, nil
	}
	switch ext {
	case ".txt", ".md", ".markdown", ".html", ".htm", ".css", ".json", ".csv", ".svg":
		if !utf8.Valid(data) || bytes.ContainsRune(data, '\x00') {
			return out, errors.New("reference text must be UTF-8")
		}
		budget.text += len(data)
		if budget.text > maxReferenceText {
			return out, errors.New("reference text exceeds 256 KiB in total")
		}
		out.MimeType = "text/plain"
		out.Text = string(data)
		return out, nil
	default:
		return out, errors.New("unsupported reference; use PNG, JPEG, GIF, UTF-8 text, Markdown, HTML, CSS, JSON, CSV, or SVG")
	}
}

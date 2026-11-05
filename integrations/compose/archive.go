package compose

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// ArchiveLimits are one caller's bounds on a ZIP read in memory. The hardened
// rules (no path leaving the archive, no nested archive opened, no link) are
// the same for every caller; only these numbers and the treatment of members
// that cannot be used differ.
type ArchiveLimits struct {
	MaxEntries       int
	MaxUnpackedBytes int64
	MaxTextBytes     int64 // total across all text members
	MaxImages        int   // 0 means no cap
	MaxPixels        int64
	// SkipUnsupported names a member that is not a picture or text file in
	// Archive.Skipped instead of failing the whole archive.
	SkipUnsupported bool
	// SkipUnusable is composition's mode: a member that cannot be used -- a
	// picture past MaxPixels, an archive inside the archive (never opened),
	// text past MaxTextBytes -- is named in Archive.Skipped, in composition's
	// own words, and the rest of the archive is used. Without it, any of them
	// fails the whole archive, as the Materializer has always read one.
	SkipUnusable bool
	// reportUnpacked, when set, is the total named in the over-budget sentence
	// while MaxUnpackedBytes is a smaller remainder of a shared budget.
	reportUnpacked int64
}

func (l ArchiveLimits) unpackedWords() string {
	if l.reportUnpacked > 0 {
		return sizeWords(l.reportUnpacked)
	}
	return sizeWords(l.MaxUnpackedBytes)
}

// MaterializerArchiveLimits are what the Materializer has always applied to a
// reference ZIP.
var MaterializerArchiveLimits = ArchiveLimits{MaxEntries: 64, MaxUnpackedBytes: 16 << 20, MaxTextBytes: 256 << 10, MaxImages: 8, MaxPixels: 24_000_000}

// CompositionArchiveLimits are what email composition applies to an uploaded ZIP.
var CompositionArchiveLimits = ArchiveLimits{MaxEntries: 256, MaxUnpackedBytes: 64 << 20, MaxTextBytes: 4 << 20, MaxPixels: 24_000_000, SkipUnsupported: true, SkipUnusable: true}

// reasonNestedArchive is why composition leaves out an archive inside a ZIP.
const reasonNestedArchive = "an archive inside the ZIP, which is not opened"

// ArchiveMember is one readable file. Path is as written in the archive, with
// forward slashes, so a/x.txt and b/x.txt stay distinct.
type ArchiveMember struct {
	Path, MIME string
	Data       []byte
}

// SkippedMember is a file that was left out, with a short reason a person can read.
type SkippedMember struct{ Path, Reason string }

// Archive is the result of ReadArchive.
type Archive struct {
	Members []ArchiveMember
	Skipped []SkippedMember
}

// ReadArchive inspects a ZIP only in memory: no path is created, no URL is
// fetched, nothing is executed. Directory entries are not members but, as
// ever, count toward MaxEntries. Finder metadata (__MACOSX, .DS_Store) is
// ignored silently and never opened.
func ReadArchive(data []byte, limits ArchiveLimits) (Archive, error) {
	var out Archive
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return out, fmt.Errorf("invalid ZIP: %w", err)
	}
	if len(zr.File) > limits.MaxEntries {
		return out, fmt.Errorf("ZIP contains more than %d entries", limits.MaxEntries)
	}
	seen := map[string]bool{}
	var unpacked, text int64
	images := 0
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !safeReferencePath(name) || seen[name] {
			return out, fmt.Errorf("ZIP has an unsafe or duplicate member path %q", f.Name)
		}
		seen[name] = true
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return out, fmt.Errorf("ZIP member %q is not a regular file", f.Name)
		}
		if strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store" {
			continue
		}
		if strings.ToLower(path.Ext(name)) == ".zip" {
			if limits.SkipUnusable {
				out.Skipped = append(out.Skipped, SkippedMember{Path: name, Reason: reasonNestedArchive})
				continue
			}
			return out, fmt.Errorf("ZIP member %q: %w", name, errNestedArchive)
		}
		if limits.SkipUnsupported && !supportedExt(name, limits) {
			out.Skipped = append(out.Skipped, SkippedMember{Path: name, Reason: unsupportedReason(name)})
			continue
		}
		remaining := limits.MaxUnpackedBytes - unpacked
		if remaining <= 0 || f.UncompressedSize64 > uint64(remaining) {
			return out, unpackedError(limits)
		}
		r, err := f.Open()
		if err != nil {
			return out, err
		}
		body, readErr := io.ReadAll(io.LimitReader(r, remaining+1))
		closeErr := r.Close()
		if readErr != nil {
			return out, readErr
		}
		if closeErr != nil {
			return out, closeErr
		}
		if int64(len(body)) > remaining {
			return out, unpackedError(limits)
		}
		unpacked += int64(len(body))
		member, reason, err := classifyMember(name, body, limits)
		if err != nil {
			return out, fmt.Errorf("ZIP member %q: %w", name, err)
		}
		if reason != "" {
			out.Skipped = append(out.Skipped, SkippedMember{Path: name, Reason: reason})
			continue
		}
		if strings.HasPrefix(member.MIME, "image/") {
			images++
			if limits.MaxImages > 0 && images > limits.MaxImages {
				return out, fmt.Errorf("ZIP member %q: %w", name, imageCountError(limits))
			}
		} else {
			text += int64(len(body))
			if text > limits.MaxTextBytes {
				if limits.SkipUnusable {
					text -= int64(len(body))
					out.Skipped = append(out.Skipped, SkippedMember{Path: name, Reason: "the ZIP holds more than " + sizeWords(limits.MaxTextBytes) + " of text"})
					continue
				}
				return out, fmt.Errorf("ZIP member %q: %w", name, textTotalError(limits))
			}
		}
		out.Members = append(out.Members, member)
	}
	return out, nil
}

var errNestedArchive = errors.New("nested archives are not supported; provide their files directly")

// unpackedError is the refusal of an archive that unpacks to more than its
// budget: in composition's words when the limits are composition's.
func unpackedError(l ArchiveLimits) error {
	if l.SkipUnusable {
		return fmt.Errorf("it unpacks to more than %s", l.unpackedWords())
	}
	return fmt.Errorf("reference contents exceed %s expanded", l.unpackedWords())
}

func imageCountError(l ArchiveLimits) error {
	n := fmt.Sprint(l.MaxImages)
	if l.MaxImages == 8 {
		n = "eight"
	}
	return fmt.Errorf("use up to %s reference images per composition", n)
}

func textTotalError(l ArchiveLimits) error {
	return fmt.Errorf("reference text exceeds %s in total", sizeWords(l.MaxTextBytes))
}

func sizeWords(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d KiB", n>>10)
}

func isPictureExt(ext string) bool {
	return ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif"
}

func isTextExt(ext string, l ArchiveLimits) bool {
	switch ext {
	case ".txt", ".md", ".markdown", ".html", ".htm", ".css", ".json", ".csv":
		return true
	case ".svg":
		// The Materializer reads svg as text; an email cannot carry it as a picture.
		return !l.SkipUnsupported
	}
	return false
}

func supportedExt(name string, l ArchiveLimits) bool {
	ext := strings.ToLower(path.Ext(name))
	return isPictureExt(ext) || isTextExt(ext, l)
}

func unsupportedReason(name string) string {
	if strings.ToLower(path.Ext(name)) == ".svg" {
		return "svg is not a picture an email can carry"
	}
	return "not a picture or text file"
}

// classifyMember applies the content rules to one member. It returns either a
// member, a skip reason (only when limits.SkipUnsupported or SkipUnusable), or
// an error.
func classifyMember(name string, data []byte, l ArchiveLimits) (ArchiveMember, string, error) {
	m := ArchiveMember{Path: name}
	skip := func(reason string, err error) (ArchiveMember, string, error) {
		if l.SkipUnsupported {
			return m, reason, nil
		}
		return m, "", err
	}
	if len(data) == 0 {
		return skip("empty file", errors.New("reference is empty"))
	}
	ext := strings.ToLower(path.Ext(name))
	if bytes.HasPrefix(data, []byte{'P', 'K'}) || ext == ".zip" {
		if l.SkipUnusable {
			return m, reasonNestedArchive, nil
		}
		return m, "", errNestedArchive
	}
	switch {
	case isPictureExt(ext):
		config, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return skip("not a valid picture", errors.New("image content is invalid"))
		}
		if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > l.MaxPixels {
			if l.SkipUnusable {
				return m, fmt.Sprintf("a picture over %d million pixels", l.MaxPixels/1_000_000), nil
			}
			return m, "", fmt.Errorf("reference image exceeds %d million pixels", l.MaxPixels/1_000_000)
		}
		m.MIME = "image/" + format
		m.Data = data
		return m, "", nil
	case isTextExt(ext, l):
		if !utf8.Valid(data) || bytes.ContainsRune(data, '\x00') {
			return skip("not valid UTF-8 text", errors.New("reference text must be UTF-8"))
		}
		m.MIME = "text/plain"
		m.Data = data
		return m, "", nil
	}
	return skip(unsupportedReason(name), errors.New("unsupported reference; use PNG, JPEG, GIF, UTF-8 text, Markdown, HTML, CSS, JSON, CSV, or SVG"))
}

package pipelinesteps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"sort"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// artifacts.go -- a step's artifacts, out of its frame and into files (epic
// memql#5478, task #5495; design record D11: "artifacts: paths upload as
// Library files owned by the pipeline owner").
//
// The archive is the step's, and the step is code from a repository: what
// arrives between the markers is whatever the step's stdout said, so it is
// read as hostile input (Review Focus 5). Nothing here touches a filesystem
// -- the files come back as bytes for the Library -- and the bounds are the
// point rather than the parsing, the shape integrations/workbench's
// build_archive.go set for a build's tree:
//
//   - A NAME must be relative and stay inside the working copy: an absolute
//     name, a ".." element or a NUL is refused, a backslash counting as a
//     separator.
//   - Only REGULAR FILES come back. A symbolic link, a hard link, a device
//     or a FIFO is refused rather than followed: a link's target is not the
//     step's to name.
//   - Only DECLARED paths come back. The wrapper tars exactly the declared
//     paths, so an entry outside them is not an artifact, whoever put it
//     there.
//   - The total is bounded BEFORE a byte is read: every entry's header size
//     counts against the limit, kept or not, so an archive that inflates
//     past it is refused by its header rather than inflated; and the entry
//     and file counts are bounded too, so a frame of a million empty files
//     can become neither a million allocations nor a million Library files.
//   - A header's size is a CLAIM. A negative one makes the archive
//     unreadable; a header-only entry (a directory, a link, a device, a FIFO)
//     costs nothing whatever it claims, because archive/tar reads no data for
//     one; and an entry's buffer grows with the bytes that actually arrive,
//     so a claim bounds a read and never sizes an allocation.
//   - The whole decompressed STREAM is bounded too. archive/tar consumes PAX
//     and GNU long-name headers inside the one Next call that returns the
//     entry behind them -- each up to 1 MiB, as many as the archive holds --
//     so neither the entry count nor the byte total sees them; a stream
//     longer than its entries could account for is refused as too large.
//
// A refused entry costs only itself: it is skipped and reported, and the
// rest of the archive is still read.

// ArtifactFile is one extracted artifact: its path inside the step's working
// copy (relative, slash-separated, cleaned) and its bytes.
type ArtifactFile struct {
	Path  string
	Bytes []byte
}

// SkippedEntry is an archive entry ExtractArtifacts did not return.
type SkippedEntry struct {
	// Name is the entry's name as the archive wrote it.
	Name string
	// Reason says why, in words.
	Reason string
}

// SkippedEntriesError reports the entries ExtractArtifacts skipped. It is
// returned BESIDE the files it did extract: keep the files, and make the
// report a note.
type SkippedEntriesError struct {
	// Entries are the first skipped entries, in archive order.
	Entries []SkippedEntry
	// More counts the skipped entries beyond those listed.
	More int
}

func (e *SkippedEntriesError) Error() string {
	parts := make([]string, 0, len(e.Entries)+1)
	for _, s := range e.Entries {
		parts = append(parts, fmt.Sprintf("%q (%s)", s.Name, s.Reason))
	}
	if e.More > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", e.More))
	}
	return "pipelinesteps: artifact entries skipped: " + strings.Join(parts, ", ")
}

// ErrArtifactsTooLarge is the refusal of an archive beyond its limits; its
// text is the code the step reports, pl.CodeArtifactTooLarge.
var ErrArtifactsTooLarge = errors.New(pl.CodeArtifactTooLarge)

const (
	// extractMaxFiles bounds the files one step's artifacts may hold: each
	// becomes a Library file.
	extractMaxFiles = 1024
	// extractMaxEntries bounds the entries read, directories and refused
	// entries included.
	extractMaxEntries = 4 * extractMaxFiles
	// extractMaxReported bounds the skipped entries listed by name.
	extractMaxReported = 32
	// extractEntryOverhead is what one entry may add to the tar stream
	// beyond its data: its 512-byte header, up to 511 bytes padding the data
	// to a block, and a long name or PAX header of up to about 3 KiB.
	extractEntryOverhead = 4 << 10
	// extractStreamSlack covers the end-of-archive blocks and the zeros tar
	// pads its last record with (10 KiB at the default blocking factor).
	extractStreamSlack = 64 << 10
	// extractBodyChunk is the first buffer an entry's bytes go into; it
	// doubles as bytes arrive, up to the entry's claimed size.
	extractBodyChunk = 64 << 10
)

// errExtractStream refuses a decompressed stream past its budget.
var errExtractStream = fmt.Errorf("%w: the archive's tar stream is longer than its entries can account for", ErrArtifactsTooLarge)

// ExtractArtifacts unpacks a tar.gz within limits: regular files only, no
// absolute paths, no "..", total bytes <= maxBytes; declared paths that
// matched nothing are returned in missing.
//
// files are the regular files under a declared path, sorted by path. A
// declared path is a file, a directory (every file under it) or a shell glob
// (what the wrapper's shell expanded it to); a declared path no returned file
// matches is in missing, in declared order -- including one whose only
// entries were refused.
//
// err is nil, a *SkippedEntriesError returned WITH files and missing, or a
// refusal that returns nothing else: an archive that cannot be read, or one
// past its limits (errors.Is(err, ErrArtifactsTooLarge)).
func ExtractArtifacts(tgz []byte, declared []string, maxBytes int64) (files []ArtifactFile, missing []string, err error) {
	if maxBytes <= 0 {
		return nil, nil, fmt.Errorf("pipelinesteps: extracting artifacts needs a positive byte limit, not %d", maxBytes)
	}
	patterns := extractPatterns(declared)
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, nil, fmt.Errorf("pipelinesteps: the artifacts are not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(&extractBudgetReader{r: gz, left: extractStreamBudget(maxBytes)})

	var (
		skipped SkippedEntriesError
		total   int64
		entries int
		byPath  = map[string]int{}
	)
	skip := func(name, reason string) {
		if len(skipped.Entries) < extractMaxReported {
			skipped.Entries = append(skipped.Entries, SkippedEntry{Name: name, Reason: reason})
		} else {
			skipped.More++
		}
	}
	for {
		hdr, nerr := tr.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		// With GODEBUG tarinsecurepath=0 the reader answers an absolute or
		// escaping name with the header AND ErrInsecurePath: a refusal of
		// that one entry, which the name check below makes too.
		insecure := hdr != nil && errors.Is(nerr, tar.ErrInsecurePath)
		if nerr != nil && !insecure {
			return nil, nil, extractReadError("", nerr)
		}
		entries++
		if entries > extractMaxEntries {
			return nil, nil, fmt.Errorf("%w: the artifact archive holds more than %d entries", ErrArtifactsTooLarge, extractMaxEntries)
		}
		if hdr.Size < 0 {
			return nil, nil, fmt.Errorf("pipelinesteps: the artifact archive cannot be read: %q declares a negative size", hdr.Name)
		}
		size := hdr.Size
		if extractHeaderOnly(hdr.Typeflag) {
			size = 0 // archive/tar reads no data for it, whatever the field says
		}
		if size > maxBytes-total {
			return nil, nil, fmt.Errorf("%w: the artifacts expand past %d bytes", ErrArtifactsTooLarge, maxBytes)
		}
		total += size

		name, why := extractEntryName(hdr.Name)
		if why == "" && insecure {
			why = "the tar reader refuses its path"
		}
		if why != "" {
			skip(hdr.Name, why)
			continue
		}
		if hdr.Typeflag == tar.TypeDir || hdr.Typeflag == tar.TypeXGlobalHeader {
			// Structure and metadata, not content: a directory's files are
			// their own entries, and a pax global header describes the ones
			// after it.
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			skip(hdr.Name, extractTypeName(hdr.Typeflag)+", not a regular file")
			continue
		}
		if name == "." || !extractDeclared(patterns, name) {
			skip(hdr.Name, "not under a declared artifact path")
			continue
		}
		at, seen := byPath[name]
		if !seen && len(files) >= extractMaxFiles {
			return nil, nil, fmt.Errorf("%w: the artifacts hold more than %d files", ErrArtifactsTooLarge, extractMaxFiles)
		}
		body, rerr := extractBody(tr, size)
		if rerr != nil {
			return nil, nil, extractReadError(hdr.Name, rerr)
		}
		if seen {
			files[at].Bytes = body // a later entry for the same path replaces it, as tar does
			continue
		}
		byPath[name] = len(files)
		files = append(files, ArtifactFile{Path: name, Bytes: body})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, p := range patterns {
		hit := false
		for _, f := range files {
			if p.matches(f.Path) {
				hit = true
				break
			}
		}
		if !hit {
			missing = append(missing, p.declared)
		}
	}
	if len(skipped.Entries) > 0 {
		return files, missing, &skipped
	}
	return files, missing, nil
}

// extractReadError is a read that failed: the stream's budget refusal as
// itself (it is ErrArtifactsTooLarge), anything else as an unreadable
// archive.
func extractReadError(name string, err error) error {
	if errors.Is(err, ErrArtifactsTooLarge) {
		return err
	}
	if name == "" {
		return fmt.Errorf("pipelinesteps: the artifact archive cannot be read: %w", err)
	}
	return fmt.Errorf("pipelinesteps: the artifact archive cannot be read at %q: %w", name, err)
}

// extractStreamBudget is how long a decompressed tar stream may be for
// maxBytes of data: the data, every entry's overhead, and the end.
func extractStreamBudget(maxBytes int64) int64 {
	const overhead = extractMaxEntries*extractEntryOverhead + extractStreamSlack
	if maxBytes > math.MaxInt64-overhead {
		return math.MaxInt64
	}
	return maxBytes + overhead
}

// extractBudgetReader is the decompressed stream, refused past its budget.
type extractBudgetReader struct {
	r    io.Reader
	left int64
}

func (b *extractBudgetReader) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, errExtractStream
	}
	if int64(len(p)) > b.left {
		p = p[:b.left+1] // one byte past the budget, to learn whether there is one
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, errExtractStream
	}
	return n, err
}

// extractHeaderOnly is archive/tar's own set of entry types that carry no
// data in the stream.
func extractHeaderOnly(flag byte) bool {
	switch flag {
	case tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeDir, tar.TypeFifo:
		return true
	}
	return false
}

// extractBody reads an entry's size bytes into a buffer that grows only as
// bytes arrive: the header's claim bounds the read, and an entry that claims
// a gigabyte and holds a kilobyte costs a kilobyte before it is refused.
func extractBody(r io.Reader, size int64) ([]byte, error) {
	body := make([]byte, 0, int(min(size, extractBodyChunk)))
	for int64(len(body)) < size {
		if len(body) == cap(body) {
			grown := make([]byte, len(body), int(min(size, 2*int64(cap(body)))))
			copy(grown, body)
			body = grown
		}
		n, err := io.ReadFull(r, body[len(body):cap(body)])
		body = body[:len(body)+n]
		if err != nil {
			return nil, err // the data stopped short of the claim, or the stream's budget ran out
		}
	}
	return body, nil
}

// extractEntryName validates one entry name and answers it cleaned, or why
// it is refused.
func extractEntryName(raw string) (string, string) {
	name := strings.ReplaceAll(raw, `\`, "/")
	switch {
	case strings.TrimSpace(name) == "":
		return "", "an empty name"
	case strings.IndexByte(name, 0) >= 0:
		return "", "a NUL byte in its name"
	case strings.HasPrefix(name, "/"):
		return "", "an absolute path"
	}
	for _, element := range strings.Split(name, "/") {
		if element == ".." {
			return "", "a \"..\" element, which escapes the working copy"
		}
	}
	clean := path.Clean(name)
	if clean != "." && !fs.ValidPath(clean) {
		return "", "not a relative path"
	}
	return clean, ""
}

// extractTypeName names an entry type a person can read.
func extractTypeName(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "a symbolic link"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeChar:
		return "a character device"
	case tar.TypeBlock:
		return "a block device"
	case tar.TypeFifo:
		return "a FIFO"
	default:
		return fmt.Sprintf("an entry of tar type %q", flag)
	}
}

// extractPattern is one declared artifact path.
type extractPattern struct {
	declared string // as the step declared it, for the missing report
	clean    string
	glob     bool
}

// extractPatterns cleans the declared paths, dropping blanks and repeats.
func extractPatterns(declared []string) []extractPattern {
	seen := map[string]bool{}
	var out []extractPattern
	for _, d := range declared {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		clean := path.Clean(d)
		out = append(out, extractPattern{declared: d, clean: clean, glob: strings.ContainsAny(clean, "*?[")})
	}
	return out
}

// matches reports whether an entry is the declared path, under it, or -- for
// a glob -- matched by it or under a directory it matched: the shell expanded
// the glob, and tar walked whatever directory it expanded to.
func (p extractPattern) matches(name string) bool {
	if p.clean == "." || name == p.clean || strings.HasPrefix(name, p.clean+"/") {
		return true
	}
	if !p.glob {
		return false
	}
	for candidate := name; ; {
		if ok, _ := path.Match(p.clean, candidate); ok {
			return true
		}
		i := strings.LastIndexByte(candidate, '/')
		if i < 0 {
			return false
		}
		candidate = candidate[:i]
	}
}

// extractDeclared reports whether any declared path covers an entry.
func extractDeclared(patterns []extractPattern, name string) bool {
	for _, p := range patterns {
		if p.matches(name) {
			return true
		}
	}
	return false
}

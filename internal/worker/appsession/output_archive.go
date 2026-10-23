package appsession

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// readOutput uses one checked descriptor for the credential scan, digest and
// retained bytes. Reopening after a scan would let a replacement file escape it.
func (p *contentPolicy) readOutput(candidate producedFile) ([]byte, string, bool, error) {
	if p == nil {
		return nil, "", false, fmt.Errorf("output policy unavailable")
	}
	if !bytes.Equal(p.secrets.apply([]byte(candidate.rel)), []byte(candidate.rel)) {
		return nil, "", true, nil
	}
	linkInfo, err := os.Lstat(candidate.path)
	if err != nil || !linkInfo.Mode().IsRegular() {
		return nil, "", false, fmt.Errorf("output disappeared or is not a regular file")
	}
	real, why := p.resolve(candidate.path)
	if why != "" {
		return nil, "", false, fmt.Errorf("%s", why)
	}
	f, info, got := openRegular(real)
	if f == nil {
		return nil, "", false, fmt.Errorf("%s", got.omitted)
	}
	defer f.Close()
	where, why := p.confirm(f, info, real)
	if why != "" {
		return nil, "", false, fmt.Errorf("%s", why)
	}
	if !p.mayCarry(harness.ContentWrite, where, nil, nil) {
		return nil, "", false, fmt.Errorf("excluded output directory")
	}
	if candidate.info != nil && (!os.SameFile(candidate.info, info) || !candidate.info.ModTime().Equal(info.ModTime())) || info.Size() != candidate.size {
		return nil, "", false, fmt.Errorf("output changed after selection")
	}
	scan := p.secrets.scanner()
	got = hashOpen(f, info, maxPushFileBytes, scan)
	if scan != nil && scan.found {
		return nil, "", true, nil
	}
	after, err := f.Stat()
	if err != nil || got.omitted != "" || got.size == nil || got.data == nil || *got.size != info.Size() || !after.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() {
		return nil, "", false, fmt.Errorf("output could not be read completely or changed during retention")
	}
	if _, why := p.confirm(f, after, real); why != "" {
		return nil, "", false, fmt.Errorf("%s", why)
	}
	return got.data, got.digest, false, nil
}

// pushProduced keeps small sets individually addressable. Larger sets use
// stored ZIP parts: exact source bytes, bounded entry metadata, no compression
// expansion and at most the existing number/bytes of Library uploads. Each
// successful part is committed to the manifest only after its upload succeeds.
func (s *session) pushProduced(ctx context.Context, library *Library, files []producedFile, manifest *outputManifest) ([]string, []string) {
	var ids, failures []string
	var used int64
	uploads := 0
	policy := s.contentPolicy()
	var buffer bytes.Buffer
	var archive *zip.Writer
	var members []outputArtifact
	var archiveBytes int64
	part := 0
	upload := func(body []byte, name string, entries []outputArtifact, bundled bool) {
		if uploads >= maxPushFiles || int64(len(body)) > maxPushFileBytes || int64(len(body)) > maxPushTotalBytes-used {
			for _, entry := range entries {
				manifest.Omitted = append(manifest.Omitted, entry.Path+" (output upload count or byte cap)")
			}
			failures = append(failures, name+" exceeds bounded output upload budget")
			return
		}
		// Charge attempts too: a failed HTTP response may follow a stored body.
		uploads++
		used += int64(len(body))
		id, err := library.PushBytes(ctx, body, name)
		if err != nil {
			for _, entry := range entries {
				manifest.Omitted = append(manifest.Omitted, entry.Path+" (Library upload failed)")
			}
			failures = append(failures, name+": "+err.Error())
			return
		}
		ids = append(ids, id)
		for _, entry := range entries {
			entry.ArtifactID = id
			manifest.Files = append(manifest.Files, entry)
		}
		if bundled {
			manifest.Archives = append(manifest.Archives, outputArchive{ArtifactID: id, Bytes: int64(len(body)), SHA256: fmt.Sprintf("sha256:%x", sha256.Sum256(body))})
		}
	}
	flush := func() {
		if archive == nil {
			return
		}
		if err := archive.Close(); err != nil {
			failures = append(failures, "output archive: "+err.Error())
			for _, entry := range members {
				manifest.Omitted = append(manifest.Omitted, entry.Path+" (archive creation failed)")
			}
		} else {
			part++
			upload(buffer.Bytes(), fmt.Sprintf("app-session-%s-outputs-%03d.zip", sanitizeLedgerName(s.id), part), members, true)
		}
		archive = nil
		members = nil
		buffer = bytes.Buffer{}
		archiveBytes = 0
	}
	for _, file := range files {
		data, digest, held, err := policy.readOutput(file)
		if held {
			note := file.rel + " (it holds this session's credential)"
			manifest.Omitted = append(manifest.Omitted, note)
			s.logger.Warn("app session output not pushed", "file", note)
			_ = s.emitChunk(StreamStderr, []byte("[memql] not pushed to the Library: "+note+"\n"))
			continue
		}
		if err != nil {
			manifest.Omitted = append(manifest.Omitted, file.rel+" ("+err.Error()+")")
			failures = append(failures, file.rel+": "+err.Error())
			continue
		}
		entry := outputArtifact{Path: filepath.ToSlash(file.rel), Bytes: int64(len(data)), SHA256: digest}
		if !filepath.IsLocal(file.rel) || strings.Contains(entry.Path, "\\") || len(entry.Path) > 65535 {
			manifest.Omitted = append(manifest.Omitted, file.rel+" (unsafe archive path)")
			failures = append(failures, "output has unsafe archive path")
			continue
		}
		// Store with a known CRC/length: 30-byte local header, 46-byte
		// central header, two names, and a 22-byte final directory record.
		entryBytes := entry.Bytes + 76 + 2*int64(len(entry.Path))
		if len(files) <= maxPushFiles || entryBytes+22 > maxPushFileBytes {
			flush()
			upload(data, artifactName(file.rel), []outputArtifact{entry}, false)
			continue
		}
		if archive != nil && archiveBytes+entryBytes > maxPushFileBytes {
			flush()
		}
		if archive == nil {
			archive = zip.NewWriter(&buffer)
			archiveBytes = 22
		}
		header := &zip.FileHeader{Name: entry.Path, Method: zip.Store, Flags: 0x800, CRC32: crc32.ChecksumIEEE(data), CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data))}
		writer, err := archive.CreateRaw(header)
		if err == nil {
			_, err = writer.Write(data)
		}
		if err != nil { // bytes.Buffer writes do not fail; retain an honest gap if ZIP rejects a header.
			manifest.Omitted = append(manifest.Omitted, entry.Path+" (archive entry failed)")
			failures = append(failures, entry.Path+": "+err.Error())
			continue
		}
		entry.ArchiveEntry = entry.Path
		members = append(members, entry)
		archiveBytes += entryBytes
	}
	flush()
	return ids, failures
}

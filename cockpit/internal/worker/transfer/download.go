package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

type downloadState struct {
	ID, Path, ETag, SHA256, Identity, Partial string
	Size, BlockBytes                          int64
	Chunks                                    []Chunk
}

func (r *Runner) download(ctx context.Context, j Job) (Job, error) {
	if r.CheckWrite == nil {
		return j, errors.New("this machine has not allowed backup downloads")
	}
	if e := r.CheckWrite(j.Path); e != nil {
		return j, e
	}
	if j.Size < 0 || j.Size > MaxBytes || len(j.SHA256) != 64 || j.ETag == "" {
		return j, errors.New("download has no verified source receipt")
	}
	if j.State == "waiting" || j.State == "error" {
		var e error
		j, e = r.Action(ctx, j.ID, "resume")
		if e != nil {
			return j, e
		}
	}
	dir, base := filepath.Dir(j.Path), filepath.Base(j.Path)
	root, e := os.OpenRoot(dir)
	if e != nil {
		return j, e
	}
	defer root.Close()
	partial := "." + base + "." + j.ID + ".partial"
	journal := filepath.Join(r.StateDir, "transfers", j.ID+".download.json")
	state := downloadState{ID: j.ID, Path: j.Path, ETag: j.ETag, SHA256: j.SHA256, Size: j.Size, BlockBytes: r.blockSize()}
	old, e := readCheckpoint(journal)
	known := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return j, e
	}
	if known {
		if len(old) > 8<<20 || json.Unmarshal(old, &state) != nil || state.ID != j.ID || state.Path != j.Path || state.ETag != j.ETag || state.SHA256 != j.SHA256 || state.Size != j.Size || state.BlockBytes != r.blockSize() {
			return j, errors.New("download checkpoint does not match this backup")
		}
	}
	if existing, err := root.Lstat(base); err == nil {
		// A completed no-replace link can survive a lost report. Adopt only our
		// previously journalled inode, with every byte independently rehashed.
		if !known || !existing.Mode().IsRegular() || state.Identity != fileIdentity(existing) {
			return j, errors.New("the download destination already exists; choose another path")
		}
		f, e := root.Open(base)
		if e != nil {
			return j, e
		}
		digest, e := hashFile(ctx, f, j.Size)
		f.Close()
		if e != nil || digest != j.SHA256 {
			return j, errors.New("the existing destination differs from this backup")
		}
		e = r.Control.Call(ctx, "Report", map[string]any{"id": j.ID, "state": "complete", "offset": j.Size}, &j)
		return j, e
	} else if !errors.Is(err, os.ErrNotExist) {
		return j, err
	}
	if known && state.Partial != "" {
		if filepath.Base(state.Partial) != state.Partial || !strings.HasPrefix(state.Partial, "."+base+"."+j.ID+".") {
			return j, errors.New("invalid partial download path")
		}
		partial = state.Partial
	}
	if known && state.Identity == "" {
		known = false
	}
	if known {
		if _, err := root.Lstat(partial); errors.Is(err, os.ErrNotExist) {
			known = false
		}
	}
	var f *os.File
	if known {
		info, e := root.Lstat(partial)
		if e != nil {
			return j, errors.New("the partial download is missing; choose a new download destination")
		}
		if !info.Mode().IsRegular() || fileIdentity(info) != state.Identity {
			return j, errors.New("the partial download was replaced")
		}
		f, e = root.OpenFile(partial, os.O_RDWR, 0600)
		if e != nil {
			return j, e
		}
	} else {
		// Persist intent before creating bytes. If creation's identity receipt is
		// lost, a new random partial name lets recovery continue without touching
		// any pre-existing file. The abandoned pre-receipt file has no bytes.
		partial = "." + base + "." + j.ID + "." + uuid.NewString() + ".partial"
		state.Partial, state.Identity, state.Chunks = partial, "", nil
		payload, _ := json.Marshal(state)
		if e = atomicFile(journal, payload); e != nil {
			return j, e
		}
		f, e = root.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return j, errors.New("a partial download already exists or cannot be created")
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			return j, e
		}
		state.Identity = fileIdentity(info)
		payload, _ = json.Marshal(state)
		if e = atomicFile(journal, payload); e != nil {
			f.Close()
			return j, e
		}
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || fileIdentity(opened) != state.Identity || !opened.Mode().IsRegular() {
		return j, errors.New("the partial download changed while opening")
	}
	// Reverify the durable prefix on every process recovery. Trusting a byte
	// count alone would adopt a torn write or a modified partial file.
	var offset int64
	valid := 0
	for _, c := range state.Chunks {
		if c.Size <= 0 || c.Size > r.blockSize() || offset+c.Size > j.Size {
			break
		}
		b := make([]byte, c.Size)
		if _, e = f.ReadAt(b, offset); e != nil || checksum(b) != c {
			break
		}
		offset += c.Size
		valid++
	}
	state.Chunks = state.Chunks[:valid]
	if e = f.Truncate(offset); e != nil {
		return j, e
	}
	for offset < j.Size {
		if e = ctx.Err(); e != nil {
			return j, e
		}
		if e = r.CheckWrite(j.Path); e != nil {
			return j, e
		}
		var block DownloadBlock
		if e = r.Control.Call(ctx, "DownloadBlock", map[string]any{"id": j.ID, "ordinal": offset / r.blockSize()}, &block); e != nil {
			return j, e
		}
		size := min(r.blockSize(), j.Size-offset)
		if block.Offset != offset || block.Size != size || len(block.SHA256) != 64 {
			return j, errors.New("download range differs from its receipt")
		}
		resp, e := r.request(ctx, block.Request, nil)
		if e != nil {
			return j, e
		}
		expected := fmt.Sprintf("bytes %d-%d/%d", offset, offset+size-1, j.Size)
		if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("ETag") != j.ETag || resp.Header.Get("Content-Range") != expected || resp.ContentLength != size {
			resp.Body.Close()
			return j, errors.New("storage returned a different download range or version")
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, size+1))
		resp.Body.Close()
		if e != nil || int64(len(data)) != size {
			return j, errors.New("download interrupted; verified progress is kept")
		}
		c := checksum(data)
		if c.SHA256 != block.SHA256 {
			return j, errors.New("download checksum failed; the block was not saved")
		}
		named, e := root.Lstat(partial)
		if e != nil || fileIdentity(named) != state.Identity || !named.Mode().IsRegular() {
			return j, errors.New("the partial download changed")
		}
		if _, e = f.WriteAt(data, offset); e != nil {
			return j, e
		}
		if e = f.Sync(); e != nil {
			return j, e
		}
		state.Chunks = append(state.Chunks, c)
		payload, _ := json.Marshal(state)
		if e = atomicFile(journal, payload); e != nil {
			return j, e
		}
		offset += size
		if offset >= j.Committed {
			if e = r.Control.Call(ctx, "Report", map[string]any{"id": j.ID, "state": "downloading", "offset": offset}, &j); e != nil {
				return j, e
			}
		}
	}
	digest, e := hashFile(ctx, f, j.Size)
	if e != nil || digest != j.SHA256 {
		return j, errors.New("complete download checksum failed")
	}
	if e = f.Sync(); e != nil {
		return j, e
	}
	if e = r.CheckWrite(j.Path); e != nil {
		return j, e
	}
	// Link is atomic and fails if the destination exists. Both names are in the
	// same directory; the old .partial is removed only after the final link is durable.
	named, e := root.Lstat(partial)
	if e != nil || !os.SameFile(opened, named) {
		return j, errors.New("the partial download changed before completion")
	}
	if e = root.Link(partial, base); e != nil {
		return j, errors.New("cannot finalize without replacing an existing file; choose another destination")
	}
	linked, e := root.Lstat(base)
	if e != nil || !os.SameFile(opened, linked) {
		return j, errors.New("download destination changed during completion")
	}
	if e = syncDirectory(dir); e != nil {
		return j, e
	}
	if e = root.Remove(partial); e != nil {
		return j, e
	}
	if e = syncDirectory(dir); e != nil {
		return j, e
	}
	e = r.Control.Call(ctx, "Report", map[string]any{"id": j.ID, "state": "complete", "offset": j.Size}, &j)
	return j, e
}
func hashFile(ctx context.Context, f *os.File, size int64) (string, error) {
	if _, e := f.Seek(0, io.SeekStart); e != nil {
		return "", e
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	var read int64
	for {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		n, e := f.Read(buf)
		if n > 0 {
			read += int64(n)
			if read > size {
				return "", errors.New("file grew")
			}
			h.Write(buf[:n])
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return "", e
		}
	}
	if read != size {
		return "", errors.New("file size differs")
	}
	return strings.ToLower(hex.EncodeToString(h.Sum(nil))), nil
}

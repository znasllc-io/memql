package transfer

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{6,64}$`)

type Chunk struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MD5    string `json:"md5"`
}
type Manifest struct {
	Version  int     `json:"version"`
	Path     string  `json:"path"`
	Size     int64   `json:"size"`
	ModNano  int64   `json:"modNano"`
	Identity string  `json:"identity"`
	SHA256   string  `json:"sha256"`
	Chunks   []Chunk `json:"chunks"`
}

func checksum(b []byte) Chunk {
	sha := sha256.Sum256(b)
	md := md5.Sum(b)
	return Chunk{Size: int64(len(b)), SHA256: hex.EncodeToString(sha[:]), MD5: base64.StdEncoding.EncodeToString(md[:])}
}
func atomicFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func syncDirectory(path string) error {
	d, e := os.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (r *Runner) manifestPath(id string) string {
	return filepath.Join(r.StateDir, "transfers", id+".source.json")
}
func (r *Runner) source(ctx context.Context, j Job) (*os.File, Manifest, error) {
	var m Manifest
	if r.CheckRead == nil {
		return nil, m, errors.New("this machine has not allowed backup reads")
	}
	if err := r.CheckRead(j.Path); err != nil {
		return nil, m, err
	}
	// Root.Open anchors subsequent resolution and refuses links outside the
	// authorized parent. The leaf must itself be a regular file, not a symlink.
	info, err := os.Lstat(j.Path)
	if err != nil {
		return nil, m, err
	}
	if !info.Mode().IsRegular() {
		return nil, m, errors.New("select a regular archive or a folder to sync")
	}
	if info.Size() < 0 || info.Size() > MaxBytes {
		return nil, m, errors.New("a backup cannot exceed 1 TB")
	}
	root, err := os.OpenRoot(filepath.Dir(j.Path))
	if err != nil {
		return nil, m, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(j.Path))
	if err != nil {
		return nil, m, err
	}
	fresh, err := f.Stat()
	if err != nil || !os.SameFile(info, fresh) {
		f.Close()
		return nil, m, errors.New("the source changed while opening it")
	}
	cached, err := readCheckpoint(r.manifestPath(j.ID))
	if err == nil && len(cached) < 8<<20 && json.Unmarshal(cached, &m) == nil && m.Version == 1 && m.Path == j.Path && m.Size == info.Size() && m.ModNano == info.ModTime().UnixNano() && m.Identity == fileIdentity(info) && validManifest(m, r.blockSize()) {
		return f, m, nil
	}
	m = Manifest{Version: 1, Path: j.Path, Size: info.Size(), ModNano: info.ModTime().UnixNano(), Identity: fileIdentity(info)}
	h := sha256.New()
	buffer := make([]byte, r.blockSize())
	for remaining := m.Size; remaining > 0; {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, m, err
		}
		b := buffer[:min(int64(len(buffer)), remaining)]
		if _, err = io.ReadFull(f, b); err != nil {
			f.Close()
			return nil, m, err
		}
		h.Write(b)
		m.Chunks = append(m.Chunks, checksum(b))
		remaining -= int64(len(b))
	}
	after, err := f.Stat()
	if err != nil || !sameSource(info, after) {
		f.Close()
		return nil, m, errors.New("the source changed while checksums were being prepared")
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	data, err := json.Marshal(m)
	if err == nil {
		err = atomicFile(r.manifestPath(j.ID), data)
	}
	if err != nil {
		f.Close()
		return nil, m, err
	}
	return f, m, nil
}
func validManifest(m Manifest, size int64) bool {
	if len(m.SHA256) != 64 || m.Size < 0 || m.Size > MaxBytes || len(m.Chunks) != int((m.Size+size-1)/size) {
		return false
	}
	for n, b := range m.Chunks {
		if b.Size != min(size, m.Size-int64(n)*size) || len(b.SHA256) != 64 || len(b.MD5) != 24 {
			return false
		}
	}
	return true
}
func sameSource(a, b os.FileInfo) bool {
	return b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// A corrupt local cache must not allocate memory proportional to its disk size.
func readCheckpoint(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if len(data) > 8<<20 {
		return nil, errors.New("backup checkpoint is too large")
	}
	return data, err
}

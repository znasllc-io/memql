// ocisnapshot retains a public, digest-pinned OCI index for later reviewed
// publication. It has no registry write path and uses no ambient credentials.
package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

const byteLimit = int64(2 << 30)

type sourcePin struct {
	Version   string   `json:"version"`
	Source    string   `json:"source"`
	Platforms []string `json:"platforms"`
}

func parsePin(b []byte) (sourcePin, name.Digest, error) {
	var pin sourcePin
	if len(b) > 4096 {
		return pin, name.Digest{}, errors.New("source pin exceeds byte limit")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return pin, name.Digest{}, errors.New("source pin must be an object")
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		key, ok := tok.(string)
		if err != nil || !ok || seen[key] || (key != "source" && key != "version" && key != "platforms") {
			return pin, name.Digest{}, errors.New("unknown or duplicate source pin field")
		}
		seen[key] = true
		if key == "platforms" {
			if err := d.Decode(&pin.Platforms); err != nil {
				return pin, name.Digest{}, errors.New("platforms must be explicit native Linux architectures")
			}
			continue
		}
		var value string
		if err := d.Decode(&value); err != nil {
			return pin, name.Digest{}, errors.New("source pin values must be strings")
		}
		if key == "source" {
			pin.Source = value
		} else {
			pin.Version = value
		}
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return pin, name.Digest{}, errors.New("trailing source pin data")
	}
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(pin.Version) {
		return pin, name.Digest{}, errors.New("source pin requires an exact component version")
	}
	if len(pin.Platforms) != 2 || pin.Platforms[0] != "linux/amd64" || pin.Platforms[1] != "linux/arm64" {
		return pin, name.Digest{}, errors.New("fixture pin must explicitly select linux/amd64 and linux/arm64")
	}
	ref, err := name.NewDigest(pin.Source, name.StrictValidation)
	if err != nil || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(ref.DigestStr()) ||
		(ref.RegistryStr() != "index.docker.io" && ref.RegistryStr() != "ghcr.io") ||
		strings.ContainsAny(pin.Source, "?#\\\r\n") {
		return pin, name.Digest{}, errors.New("source must be a public Docker Hub or GHCR sha256 reference")
	}
	return pin, ref, nil
}

// Bound total network response bytes, including decomposed layers, even when
// Content-Length is absent or dishonest. The whole operation also has a deadline.
type readTransport struct {
	inner     http.RoundTripper
	remaining atomic.Int64
	requests  atomic.Int64
}

func (t *readTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if (r.Method != "GET" && r.Method != "HEAD") || r.URL.Scheme != "https" || t.requests.Add(1) > 8192 {
		return nil, errors.New("snapshot permits only bounded HTTPS reads")
	}
	resp, err := t.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &budgetBody{ReadCloser: resp.Body, remaining: &t.remaining}
	return resp, nil
}

type budgetBody struct {
	io.ReadCloser
	remaining *atomic.Int64
}

func (b *budgetBody) Read(p []byte) (int, error) {
	left := b.remaining.Load()
	if left < 0 {
		return 0, errors.New("snapshot download exceeds byte limit")
	}
	if int64(len(p)) > left+1 {
		p = p[:left+1]
	}
	n, err := b.ReadCloser.Read(p)
	if b.remaining.Add(-int64(n)) < 0 {
		return 0, errors.New("snapshot download exceeds byte limit")
	}
	return n, err
}

func snapshot(ctx context.Context, pin sourcePin, ref name.Digest, out string) (err error) {
	if _, err := os.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("snapshot output must not already exist")
	}
	stage, err := os.MkdirTemp(filepath.Dir(out), ".memql-oci-snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	transport := remote.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxResponseHeaderBytes = 64 << 10
	defer transport.CloseIdleConnections()
	bounded := &readTransport{inner: transport}
	bounded.remaining.Store(byteLimit)
	desc, err := remote.Get(ref, remote.WithAuth(authn.Anonymous), remote.WithTransport(bounded), remote.WithContext(ctx), remote.WithJobs(2))
	if err != nil {
		return errors.New("public source index download failed")
	}
	sourceHash := sha256.Sum256(desc.Manifest)
	if desc.MediaType != "application/vnd.oci.image.index.v1+json" || "sha256:"+hex.EncodeToString(sourceHash[:]) != ref.DigestStr() {
		return errors.New("source does not name the exact OCI index")
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return errors.New("source index could not be decoded")
	}
	idx, err = nativePlatforms(idx)
	if err != nil {
		return err
	}
	selectedDigest, err := idx.Digest()
	if err != nil {
		return errors.New("selected index identity could not be computed")
	}
	layoutDir := filepath.Join(stage, "layout")
	l, err := layout.Write(layoutDir, empty.Index)
	if err != nil {
		return err
	}
	if err := l.AppendIndex(idx); err != nil {
		if bounded.remaining.Load() < 0 {
			return errors.New("selected index exceeds download byte limit")
		}
		return errors.New("complete selected index download failed")
	}
	archivePath := filepath.Join(stage, "image.oci.tar")
	f, err := os.OpenFile(archivePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	var retained int64
	err = filepath.WalkDir(layoutDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("layout contains a non-regular entry")
		}
		retained += info.Size()
		if retained > byteLimit {
			return errors.New("layout exceeds byte limit")
		}
		rel, err := filepath.Rel(layoutDir, path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0600, Size: info.Size(), Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	want := ociregistry.Expected{ArchiveSHA256: "sha256:" + hex.EncodeToString(h.Sum(nil)), ArchiveSize: size, ImageDigest: selectedDigest.String(), Platform: "oci-index"}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	verified, err := ociregistry.Verify(ctx, f, want, ociregistry.Limits{})
	if err != nil {
		return fmt.Errorf("complete index verification failed: %w", err)
	}
	if err := verified.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.RemoveAll(layoutDir); err != nil {
		return err
	}
	metadata, err := json.MarshalIndent(map[string]any{"containerimage.digest": want.ImageDigest, "platform": want.Platform, "source": pin.Source, "sourceIndexDigest": ref.DigestStr(), "platforms": pin.Platforms, "version": pin.Version, "archiveSHA256": want.ArchiveSHA256, "archiveSize": want.ArchiveSize}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "metadata.json"), append(metadata, '\n'), 0600); err != nil {
		return err
	}
	if _, err := os.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("snapshot output appeared during download")
	}
	return os.Rename(stage, out)
}

func main() {
	pinPath := flag.String("pin", "", "reviewed public source pin")
	out := flag.String("out", "", "new output directory")
	flag.Parse()
	if *pinPath == "" || *out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "pin and output are required")
		os.Exit(2)
	}
	pinFile, err := os.Open(*pinPath)
	var b []byte
	if err == nil {
		b, err = io.ReadAll(io.LimitReader(pinFile, 4097))
		err = errors.Join(err, pinFile.Close())
	}
	if err == nil {
		var pin sourcePin
		var ref name.Digest
		pin, ref, err = parsePin(b)
		if err == nil {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			defer cancel()
			err = snapshot(ctx, pin, ref, *out)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Native-platform OCI index verified and retained; publication is separate.")
}

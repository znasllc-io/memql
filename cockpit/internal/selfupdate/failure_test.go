package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedAtomicReplacementRetainsRecoveryFiles(t *testing.T) {
	f := setupFixture(t)
	run := f.c.run
	f.c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		body, err := run(ctx, name, args...)
		if len(args) == 1 && args[0] == "--version" && name != f.c.Executable && err == nil {
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
		}
		return body, err
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if r.OK || r.Updated {
		t.Fatalf("%+v", r)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(f.c.Executable), ".memql-update-*"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("recovery stage removed: %v %v", stages, err)
	}
	old, err := os.ReadFile(filepath.Join(stages[0], "previous"))
	if err != nil || string(old) != "memql 1.0.0 (computeruse)\n" {
		t.Fatalf("old executable lost: %q %v", old, err)
	}
	if f.cancels != 1 || f.restarts != 0 {
		t.Fatalf("lease was not released: cancels=%d restarts=%d", f.cancels, f.restarts)
	}
}

func TestArchiveSizeLimitCannotOverflow(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "oversized.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	writer := tar.NewWriter(gz)
	if err = writer.WriteHeader(&tar.Header{Name: "small", Size: 1, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte{0})
	if err = writer.WriteHeader(&tar.Header{Name: "huge", Size: math.MaxInt64, Mode: 0644, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	_ = gz.Close()
	_ = f.Close()
	if err = extract(archive, filepath.Join(root, "out")); err == nil || !strings.Contains(err.Error(), "unsafe entries") {
		t.Fatalf("huge member reached extraction: %v", err)
	}
}

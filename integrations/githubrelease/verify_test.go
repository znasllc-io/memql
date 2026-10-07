package githubrelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func expected(data []byte) Expected {
	return Expected{SHA256: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Size: int64(len(data))}
}

func verified(t *testing.T, data []byte) *VerifiedFile {
	t.Helper()
	v, err := Verify(context.Background(), bytes.NewReader(data), expected(data), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	return v
}

func TestVerifyOwnsExactSnapshot(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("binary\x00asset\xff")} {
		v := verified(t, data)
		dir := v.dir
		for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "asset"): 0600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("snapshot permissions", path, info, err)
			}
		}
		got, err := os.ReadFile(filepath.Join(dir, "asset"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("snapshot changed", err)
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("snapshot was not removed", err)
		}
		if err := v.Close(); err != nil {
			t.Fatal("close was not idempotent", err)
		}
	}
}

func TestVerifyRefusesReceiptMismatchAndCleansFailure(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	data := []byte("receipt bytes")
	want := expected(data)
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   Expected
		limits Limits
	}{
		{"nil reader", nil, want, Limits{}},
		{"digest syntax", bytes.NewReader(data), Expected{strings.Repeat("a", 64), want.Size}, Limits{}},
		{"negative size", bytes.NewReader(data), Expected{want.SHA256, -1}, Limits{}},
		{"wrong digest", bytes.NewReader(data), Expected{"sha256:" + strings.Repeat("b", 64), want.Size}, Limits{}},
		{"truncated", bytes.NewReader(data[:len(data)-1]), want, Limits{}},
		{"extra bytes", strings.NewReader(string(data) + "x"), want, Limits{}},
		{"read error", iotest.ErrReader(errors.New("broken stream")), want, Limits{}},
		{"lower limit", bytes.NewReader(data), want, Limits{FileBytes: 1}},
		{"negative limit", bytes.NewReader(data), want, Limits{FileBytes: -1}},
		{"ceiling", bytes.NewReader(data), want, Limits{FileBytes: 2 << 30}},
		{"receipt ceiling", bytes.NewReader(data), Expected{want.SHA256, 2 << 30}, Limits{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v, err := Verify(context.Background(), tc.reader, tc.want, tc.limits); err == nil || v != nil {
				t.Fatal("invalid input verified", v, err)
			}
			entries, err := os.ReadDir(scratch)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed verification leaked scratch", entries, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Verify(ctx, bytes.NewReader(data), want, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

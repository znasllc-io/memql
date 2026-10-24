//go:build linux || darwin

package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/transfer"
)

type deletionControl struct {
	calls int
	err   error
	file  string
}

func (c *deletionControl) Call(_ context.Context, op string, args map[string]any, _ any) error {
	if op != "SyncDelete" {
		return errors.New("unexpected operation")
	}
	c.calls++
	c.file, _ = args["fileId"].(string)
	return c.err
}

func TestSyncDeletionRequiresCompleteScanAndSameMountedRoot(t *testing.T) {
	for _, scenario := range []string{"deleted", "partial", "root-replaced", "nested-unmounted", "permission-lost", "still-present", "revision-changed", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			source := mk(t, root, "folder/data.txt")
			stamp, err := statOf(source)
			if err != nil {
				t.Fatal(err)
			}
			l := LoadLedger(t.TempDir(), "watch-001")
			l.Put(source, Record{Stamp: stamp, FileID: "file-001", VersionNumber: 1})
			scan, err := scanFolder(root, nil, true, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			scan.Entries = nil
			if scenario != "still-present" {
				if err = os.Remove(source); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "partial" {
				scan.Skipped = 1
			}
			if scenario == "root-replaced" {
				scan.RootIdentity = "replaced:root"
			}
			if scenario == "nested-unmounted" {
				r, _ := l.Get(source)
				r.Stamp.Identity = "another-device:inode"
				l.Put(source, r)
			}
			control := &deletionControl{}
			if scenario == "revision-changed" {
				control.err = errors.New("cloud copy changed")
			}
			check := func(string) error { return nil }
			if scenario == "permission-lost" {
				check = func(string) error { return errors.New("not allowed") }
			}
			m := &Manager{opts: Options{CheckPath: check}, workerID: "worker-001", transfers: &transfer.Runner{Control: control}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			m.syncDeletions(ctx, Watch{ID: "watch-001", LocalPath: root, Mode: "sync"}, l, scan)
			r, _ := l.Get(source)
			wantCall := scenario == "deleted" || scenario == "revision-changed"
			if (control.calls == 1) != wantCall {
				t.Fatalf("unsafe or missing deletion: %d", control.calls)
			}
			if r.InBin != (scenario == "deleted") {
				t.Fatalf("wrong Bin receipt: %+v", r)
			}
			if scenario == "deleted" {
				restored := LoadLedger(filepath.Dir(filepath.Dir(l.path)), "watch-001")
				rec, _ := restored.Get(source)
				if !rec.InBin {
					t.Fatal("Bin receipt did not survive a restart")
				}
				m.syncDeletions(ctx, Watch{ID: "watch-001", LocalPath: root, Mode: "sync"}, l, scan)
				if control.calls != 1 {
					t.Fatal("already confirmed deletion was repeated")
				}
			}
		})
	}
}

func TestSyncScanIncludesHiddenAndDerivedDirectories(t *testing.T) {
	root := t.TempDir()
	mk(t, root, ".hidden")
	mk(t, root, "node_modules/package/index.js")
	mk(t, root, ".git/config")
	scan, err := scanFolder(root, nil, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Entries) != 3 {
		t.Fatalf("sync silently excluded source files: %+v", scan)
	}
}

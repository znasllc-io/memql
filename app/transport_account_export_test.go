//go:build bff

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/accounts"
)

// fakeExportSource stands in for *accounts.Exports' three download methods.
type fakeExportSource struct {
	rows       map[string]accounts.Export
	resolveErr error
	recordErr  error

	resolves []string
	opened   []accounts.Export
	openArgs [][2]int64
	recorded []accounts.Export
	users    []string
	ranges   []string
}

func (f *fakeExportSource) ResolveExportForDownload(_ context.Context, id string) (accounts.Export, bool, error) {
	f.resolves = append(f.resolves, id)
	if f.resolveErr != nil {
		return accounts.Export{}, false, f.resolveErr
	}
	x, ok := f.rows[id]
	return x, ok, nil
}

func (f *fakeExportSource) OpenExport(_ context.Context, x accounts.Export, offset, length int64) (io.ReadCloser, error) {
	f.opened = append(f.opened, x)
	f.openArgs = append(f.openArgs, [2]int64{offset, length})
	return io.NopCloser(strings.NewReader("BYTES-OF-" + x.ID)), nil
}

func (f *fakeExportSource) RecordExportDownload(_ context.Context, x accounts.Export, userID, rangeAsked string) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, x)
	f.users = append(f.users, userID)
	f.ranges = append(f.ranges, rangeAsked)
	return nil
}

func sampleExport(id string) accounts.Export {
	return accounts.Export{
		ID: id, AccountID: "acct-1", AccountName: "Primary account", State: "complete",
		ObjectKey: "account-exports/" + id + ".memqlx", FileName: "primary account.memqlx",
		SizeBytes: int64(len("BYTES-OF-" + id)), SHA256: strings.Repeat("ab", 32), Attempt: 1,
	}
}

func TestAccountExportAdapterMapsTheRecord(t *testing.T) {
	x := sampleExport("exp-1")
	adapter := accountExportDownloads{exports: &fakeExportSource{rows: map[string]accounts.Export{"exp-1": x}}}
	f, ok, err := adapter.ResolveForDownload(context.Background(), "exp-1")
	if err != nil || !ok {
		t.Fatalf("resolve = %v %v", ok, err)
	}
	want := server.AccountExportFile{ID: x.ID, AccountID: x.AccountID, FileName: x.FileName,
		ObjectKey: x.ObjectKey, SHA256: x.SHA256, SizeBytes: x.SizeBytes}
	if f != want {
		t.Fatalf("mapped %+v, want %+v", f, want)
	}
}

func TestAccountExportAdapterNotOkIsNotFound(t *testing.T) {
	adapter := accountExportDownloads{exports: &fakeExportSource{rows: map[string]accounts.Export{}}}
	if _, ok, err := adapter.ResolveForDownload(context.Background(), "exp-gone"); ok || err != nil {
		t.Fatalf("an export the plug-in does not resolve answered ok=%v err=%v", ok, err)
	}
	failing := accountExportDownloads{exports: &fakeExportSource{resolveErr: errors.New("db down")}}
	if _, ok, err := failing.ResolveForDownload(context.Background(), "exp-1"); ok || err == nil {
		t.Fatalf("a lookup failure answered ok=%v err=%v, want the error", ok, err)
	}
}

// Open and RecordDownload act on the row the id names, looked up again, and
// only when it is still the file the route announced.
func TestAccountExportAdapterActsOnTheSameRow(t *testing.T) {
	x := sampleExport("exp-1")
	src := &fakeExportSource{rows: map[string]accounts.Export{"exp-1": x}}
	adapter := accountExportDownloads{exports: src}
	f, _, _ := adapter.ResolveForDownload(context.Background(), "exp-1")

	if err := adapter.RecordDownload(context.Background(), f, "v1:identity:user:p1", "bytes=0-3"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.recorded, []accounts.Export{x}) || src.users[0] != "v1:identity:user:p1" || src.ranges[0] != "bytes=0-3" {
		t.Fatalf("recorded %+v by %v with %v, want the whole row", src.recorded, src.users, src.ranges)
	}
	rc, err := adapter.Open(context.Background(), f, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if !reflect.DeepEqual(src.opened, []accounts.Export{x}) || src.openArgs[0] != [2]int64{2, 5} {
		t.Fatalf("opened %+v at %v", src.opened, src.openArgs)
	}

	// Another export's record must never carry this one's bytes, and a file
	// that changed since the headers were decided is not served.
	for name, forged := range map[string]server.AccountExportFile{
		"another object key": {ID: f.ID, AccountID: f.AccountID, FileName: f.FileName, ObjectKey: "account-exports/other.memqlx", SHA256: f.SHA256, SizeBytes: f.SizeBytes},
		"another digest":     {ID: f.ID, AccountID: f.AccountID, FileName: f.FileName, ObjectKey: f.ObjectKey, SHA256: "00", SizeBytes: f.SizeBytes},
		"another size":       {ID: f.ID, AccountID: f.AccountID, FileName: f.FileName, ObjectKey: f.ObjectKey, SHA256: f.SHA256, SizeBytes: f.SizeBytes + 1},
		"another account":    {ID: f.ID, AccountID: "acct-2", FileName: f.FileName, ObjectKey: f.ObjectKey, SHA256: f.SHA256, SizeBytes: f.SizeBytes},
		"an unknown id":      {ID: "exp-gone", ObjectKey: f.ObjectKey, SHA256: f.SHA256, SizeBytes: f.SizeBytes},
	} {
		before := len(src.opened)
		if _, err := adapter.Open(context.Background(), forged, 0, -1); err == nil {
			t.Errorf("%s: opened", name)
		}
		if err := adapter.RecordDownload(context.Background(), forged, "p", ""); err == nil {
			t.Errorf("%s: recorded", name)
		}
		if len(src.opened) != before {
			t.Errorf("%s: the plug-in was asked to open", name)
		}
	}
}

// An audit failure reaches the handler as an error, so no byte is served.
func TestAccountExportAdapterAuditFailureRefusesTheDownload(t *testing.T) {
	src := &fakeExportSource{rows: map[string]accounts.Export{"exp-1": sampleExport("exp-1")}, recordErr: errors.New("audit refused")}
	h := server.NewAccountExportHandler(server.AccountExportHandlerOptions{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Downloads: accountExportDownloads{exports: src},
	})
	person := "v1:identity:user:7d1e5b2a-0c3f-4a9e-8b6d-1f2e3a4b5c6d"
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": person, "role": "owner"})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: person, Role: auth.RoleOwner})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts/exports/exp-1/content", nil).WithContext(ctx))
	if rec.Code != http.StatusInternalServerError || len(src.opened) != 0 {
		t.Fatalf("answered %d and opened %d objects, want 500 and none", rec.Code, len(src.opened))
	}
}

// With no accounts plug-in the route is still mounted, answers 404, and the
// startup log says why once.
func TestAccountExportMountWithoutThePluginAnswers404(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	var logs bytes.Buffer
	a := &App{mux: http.NewServeMux(), Logger: slog.New(slog.NewTextHandler(&logs, nil))}

	a.mountAccountExportDownload()

	if want := []string{"GET /accounts/exports/"}; !reflect.DeepEqual(a.registeredRoutes, want) {
		t.Fatalf("registered = %v, want %v", a.registeredRoutes, want)
	}
	if n := strings.Count(logs.String(), "account export download"); n != 1 {
		t.Fatalf("logged %d lines about the missing plug-in, want 1:\n%s", n, logs.String())
	}
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts/exports/exp-1/content", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("answered %d, want 404", rec.Code)
	}
}

// THE ROUTE IS MOUNTED BY ONE CALL ON ONE NODE: every other test drives the
// mount itself, so a transportBFF that lost the line would serve no download
// with the suite green. It must also run before createHTTPServer seals the
// surface, and no other node's wiring may carry it.
func TestTheBFFMountsTheAccountExportDownload(t *testing.T) {
	source, err := os.ReadFile("transport_bff.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	mount, seal := strings.Index(text, "a.mountAccountExportDownload()"), strings.Index(text, "a.createHTTPServer()")
	if mount < 0 {
		t.Fatal("transportBFF does not mount the account export download")
	}
	if seal < 0 || mount > seal {
		t.Fatal("the mount must run before createHTTPServer seals the surface")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file == "transport_bff.go" || strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), ".mountAccountExportDownload()") {
			t.Errorf("%s mounts the account export download; it belongs on the bff only", file)
		}
	}
}

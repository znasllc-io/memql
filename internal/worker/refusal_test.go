package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// TestRefusalOf is the line between "the cluster said no" and "the
// cluster could not be reached" (memql-cockpit#427). Everything on the
// wrong side of it costs something different: a refusal read as a blip is
// the fifteen-second loop forever, and a blip read as a refusal is a
// machine that waits a minute or more for a network that came back.
func TestRefusalOf(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("worker.register: recv: %w", err) }
	cases := []struct {
		name  string
		err   error
		kind  refusalKind
		said  string
		isRef bool
	}{
		{"the interceptor refusing a token", wrap(status.Error(codes.Unauthenticated, "invalid worker token")), refusedCredential, "invalid worker token", true},
		{"an inactive token at admission", &RegisterRefusedError{Code: "register_failed", Message: "worker token is inactive"}, refusedCredential, "worker token is inactive", true},
		{"an expired token at admission", &RegisterRefusedError{Code: "register_failed", Message: "worker token expired"}, refusedCredential, "worker token expired", true},
		{"a machine removed from the fleet", &RegisterRefusedError{Code: "register_failed", Message: "worker is revoked"}, refusedRemoved, "worker is revoked", true},
		{"the status after a removal, alone", status.Error(codes.PermissionDenied, "worker is revoked"), refusedRemoved, "worker is revoked", true},
		{"the status after an expiry, alone", wrap(status.Error(codes.PermissionDenied, "worker token expired")), refusedCredential, "worker token expired", true},
		// The same PermissionDenied ends EVERY refused registration, so a
		// message the engine does not use for a refusal is not one.
		{"a permission status for anything else", status.Error(codes.PermissionDenied, "worker tokens may only call WorkerService"), "", "", false},
		{"a permission status after a failed write", status.Error(codes.PermissionDenied, "worker lookup: connection reset"), "", "", false},
		{"a registration write that failed", &RegisterRefusedError{Code: "register_failed", Message: "worker lookup: context deadline exceeded"}, "", "", false},
		// Exact, not a substring: a wrapped store error that happens to
		// say "revoked" is a write that failed, not a machine removed.
		{"a store error that mentions revoked", &RegisterRefusedError{Code: "register_failed", Message: "worker refresh registration: row revoked concurrently"}, "", "", false},
		{"a descriptor this engine will not take", &RegisterRefusedError{Code: "register_failed", Message: "capability descriptor: schemaVersion 2 not supported"}, "", "", false},
		{"a network that is down", status.Error(codes.Unavailable, "connection refused"), "", "", false},
		{"a transport reset", wrap(status.Error(codes.Unavailable, "transport is closing")), "", "", false},
		{"an EOF", wrap(io.EOF), "", "", false},
		{"a plain error", errors.New("dial tcp: lookup api.example: no such host"), "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := refusalOf(tc.err)
			if ok != tc.isRef {
				t.Fatalf("refusalOf(%v) refusal=%v, want %v", tc.err, ok, tc.isRef)
			}
			if got.kind != tc.kind || got.clusterSaid != tc.said {
				t.Fatalf("refusalOf(%v) = (%q, %q), want (%q, %q)", tc.err, got.kind, got.clusterSaid, tc.kind, tc.said)
			}
		})
	}
}

// eofStream is a cluster that ended the stream before reading Register:
// the Send answers io.EOF, and only the Recv behind it knows why.
type eofStream struct{ recvErr error }

func (s *eofStream) Send(*memqlv1.WorkerClientMessage) error     { return io.EOF }
func (s *eofStream) Recv() (*memqlv1.WorkerServerMessage, error) { return nil, s.recvErr }
func (s *eofStream) Close()                                      {}

// TestRegisterSurfacesTheStatusBehindASendEOF. grpc-go answers a Send on a
// stream the server already ended with io.EOF, and puts the reason on the
// next Recv. The handshake used to stop at "send: EOF" -- so a refused
// token looked exactly like a network blip, and no amount of classifying
// could have told them apart.
func TestRegisterSurfacesTheStatusBehindASendEOF(t *testing.T) {
	s := &eofStream{recvErr: status.Error(codes.Unauthenticated, "invalid worker token")}
	_, err := handshake(context.Background(), s, Config{Name: "m", Capabilities: []string{"HEADLESS"}, StateDir: t.TempDir()},
		nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{}, quietLogger())
	if err == nil {
		t.Fatal("a refused handshake must fail")
	}
	ref, ok := refusalOf(err)
	if !ok || ref.kind != refusedCredential || ref.clusterSaid != "invalid worker token" {
		t.Fatalf("handshake error %v classified as (%v, %+v), want a credential refusal saying %q", err, ok, ref, "invalid worker token")
	}

	// And an EOF with nothing behind it stays an EOF: not a refusal.
	s = &eofStream{recvErr: io.EOF}
	_, err = handshake(context.Background(), s, Config{Name: "m", Capabilities: []string{"HEADLESS"}, StateDir: t.TempDir()},
		nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{}, quietLogger())
	if _, ok := refusalOf(err); ok || !errors.Is(err, io.EOF) {
		t.Fatalf("a bare EOF = %v (refusal=%v), want an EOF that is not a refusal", err, ok)
	}
}

// The RegisterError is typed on the way out, so the runner can read the
// cluster's code and words rather than parse a sentence it built itself.
func TestARegisterErrorIsTyped(t *testing.T) {
	s := newScriptStream(0)
	s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_RegisterError{RegisterError: &memqlv1.RegisterError{
		Code: "register_failed", Message: "worker is revoked",
	}}})
	_, err := handshake(context.Background(), s, Config{Name: "m", Capabilities: []string{"HEADLESS"}, StateDir: t.TempDir()},
		nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{}, quietLogger())
	var refused *RegisterRefusedError
	if !errors.As(err, &refused) || refused.Code != "register_failed" || refused.Message != "worker is revoked" {
		t.Fatalf("err = %v, want a *RegisterRefusedError carrying the cluster's code and words", err)
	}
}

// TestDescribeRefusal is the block `memql worker config` prints under a
// home the cluster is refusing -- asserted on its exact words, because
// those words are the whole product for the person reading them.
func TestDescribeRefusal(t *testing.T) {
	home := Home{ID: "production", Token: "mql_wkr_prod_token_aaaaaaaaaaaa"}
	since := time.Date(2026, 9, 13, 21, 4, 0, 0, time.Local)

	got := describeRefusal(refusalRecord{
		Since: since, Kind: refusedCredential, ClusterSaid: "invalid worker token", Token: tokenFingerprint(home.Token),
	}, true, home)
	want := []string{
		"REFUSED since " + since.Format("2006-01-02 15:04 MST"),
		`The cluster did not accept this machine's token (it said "invalid worker token").`,
		"The worker stopped asking every few seconds; it asks again every few minutes.",
		"Pair this machine again with a new code from the portal:",
		"  memql worker pair <code>",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("credential refusal:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	got = describeRefusal(refusalRecord{
		Since: since, Kind: refusedRemoved, ClusterSaid: "worker is revoked", Token: tokenFingerprint(home.Token),
	}, true, home)
	want = []string{
		"REFUSED since " + since.Format("2006-01-02 15:04 MST"),
		`The cluster says this machine was removed from its fleet (it said "worker is revoked").`,
		"The worker stopped asking every few seconds; it asks again every few minutes.",
		"Pair it again to serve that cluster, or remove it here to stop asking:",
		"  memql worker pair <code>   |   memql worker unpair --cluster production",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("removed refusal:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A record for a token the home no longer holds says nothing: somebody
// already re-paired, and the worker settles it on its next connect.
// Reporting it would tell a person to fix what they just fixed.
func TestDescribeRefusalIgnoresAReplacedToken(t *testing.T) {
	home := Home{ID: "production", Token: "mql_wkr_new_token_bbbbbbbbbbbb"}
	rec := refusalRecord{Since: time.Now(), Kind: refusedCredential, ClusterSaid: "invalid worker token",
		Token: tokenFingerprint("mql_wkr_old_token_aaaaaaaaaaaa")}
	if got := describeRefusal(rec, true, home); got != nil {
		t.Fatalf("a refusal of a replaced token printed %q, want nothing", got)
	}
	if got := describeRefusal(refusalRecord{}, false, home); got != nil {
		t.Fatalf("no record printed %q, want nothing", got)
	}
}

// A worker that restarts into the same refusal has been refused since the
// FIRST time; the record keeps that time. A different token starts over.
func TestSaveRefusalKeepsTheFirstTime(t *testing.T) {
	dir := t.TempDir()
	first := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	tok := tokenFingerprint("mql_wkr_a_aaaaaaaaaaaaaa")
	if err := saveRefusal(dir, refusalRecord{Since: first, Kind: refusedCredential, ClusterSaid: "x", Token: tok}); err != nil {
		t.Fatal(err)
	}
	if err := saveRefusal(dir, refusalRecord{Since: first.Add(time.Hour), Kind: refusedCredential, ClusterSaid: "x", Token: tok}); err != nil {
		t.Fatal(err)
	}
	rec, ok := loadRefusal(dir)
	if !ok || !rec.Since.Equal(first) {
		t.Fatalf("since = %v, want the first refusal's %v", rec.Since, first)
	}
	other := tokenFingerprint("mql_wkr_b_bbbbbbbbbbbbbb")
	later := first.Add(2 * time.Hour)
	if err := saveRefusal(dir, refusalRecord{Since: later, Kind: refusedCredential, ClusterSaid: "x", Token: other}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := loadRefusal(dir); !rec.Since.Equal(later) {
		t.Fatalf("a different token's refusal kept %v, want its own %v", rec.Since, later)
	}
	info, err := os.Stat(filepath.Join(dir, refusalFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("refused.json mode %04o, want 0600", info.Mode().Perm())
	}
	if err := clearRefusal(dir); err != nil {
		t.Fatal(err)
	}
	if err := clearRefusal(dir); err != nil {
		t.Fatalf("clearing an absent record must not fail: %v", err)
	}
}

// The fingerprint names a token without carrying it.
func TestTokenFingerprintCarriesNoToken(t *testing.T) {
	tok := "mql_wkr_secret_value_aaaaaaaaaaaa"
	fp := tokenFingerprint(tok)
	if len(fp) != 12 || strings.Contains(tok, fp) || strings.Contains(fp, "mql_wkr_") {
		t.Fatalf("fingerprint %q must be 12 hex digits sharing nothing with the token", fp)
	}
	if tokenFingerprint(tok) != fp || tokenFingerprint("mql_wkr_other_aaaaaaaaaaaaaa") == fp {
		t.Fatal("the fingerprint must be stable per token and differ between tokens")
	}
}

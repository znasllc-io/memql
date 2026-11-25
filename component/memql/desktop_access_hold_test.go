package memql

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The exemption, the reference and the hold line (desktop-access design,
// sections 2, 3 and 7).

type holdRecorder struct {
	mu    sync.Mutex
	lines []AccessHoldLine
	err   error
}

func (r *holdRecorder) record(_ context.Context, line AccessHoldLine) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.lines = append(r.lines, line)
	return nil
}

func installRecorder(t *testing.T) *holdRecorder {
	t.Helper()
	r := &holdRecorder{}
	SetAccessHoldRecorder(r.record)
	t.Cleanup(func() { SetAccessHoldRecorder(nil) })
	return r
}

// holdEngine is an engine with nothing set up for anybody and the stored role
// the principal table would hold for them.
func holdEngine(stored auth.Role) *MemQLEngine {
	e := engineWithFleet(nil)
	withAppRows(e)
	e.storedRoleForTest = func(context.Context, string) (auth.Role, bool, error) { return stored, stored != "", nil }
	return e
}

func TestAnOwnerOrDeveloperIsAdmittedWithNothingSetUp(t *testing.T) {
	rec := installRecorder(t)
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper} {
		got, err := holdEngine(role).desktopAccess(personContext("ana"), "ana", "", "")
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if !got.Admitted || !got.Exempt || got.Reference != "" {
			t.Fatalf("%s: answer = %+v, want admitted and exempt with no reference", role, got)
		}
	}
	if len(rec.lines) != 0 {
		t.Fatalf("lines = %+v, want none", rec.lines)
	}
}

func TestAnAdminWithNothingSetUpIsHeld(t *testing.T) {
	installRecorder(t)
	got, err := holdEngine(auth.RoleAdmin).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Admitted || got.Exempt || !accessReferencePattern.MatchString(got.Reference) {
		t.Fatalf("answer = %+v, want held with a reference", got)
	}
}

func TestTheExemptionReadsTheStoredRoleNotAStandIn(t *testing.T) {
	installRecorder(t)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "ana", Role: auth.RoleOwner, RoleStandIn: true})
	got, err := holdEngine(auth.RoleWriter).desktopAccess(ctx, "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Admitted || got.Exempt {
		t.Fatalf("answer = %+v, want held: the stand-in role is not who she is", got)
	}
}

func TestAHeldAnswerWritesOneLineBeforeItReturnsTheReference(t *testing.T) {
	rec := installRecorder(t)
	got, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.lines) != 1 {
		t.Fatalf("lines = %+v, want exactly one", rec.lines)
	}
	line := rec.lines[0]
	if !accessReferencePattern.MatchString(line.Reference) || line.Reference != got.Reference {
		t.Fatalf("line reference %q, answer reference %q", line.Reference, got.Reference)
	}
	if line.UserId != "ana" || len(line.Reasons) == 0 {
		t.Fatalf("line = %+v, want the person and reasons", line)
	}
	if got.ReasonsDigest != personAccessDigest("ana", line.Reasons) {
		t.Fatalf("digest %q does not match the line's reasons", got.ReasonsDigest)
	}
}

func TestARecorderFailureHoldsWithNoReference(t *testing.T) {
	rec := installRecorder(t)
	rec.err = errors.New("log store down")
	got, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || got.Admitted || got.Reference != "" {
		t.Fatalf("answer = %+v, err = %v, want held with no reference", got, err)
	}
	// With no recorder installed at all, writing counts as failed.
	SetAccessHoldRecorder(nil)
	got, err = holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || got.Admitted || got.Reference != "" {
		t.Fatalf("no recorder: answer = %+v, err = %v, want held with no reference", got, err)
	}
}

func TestCheckingAgainWithTheSameReasonsWritesNothing(t *testing.T) {
	rec := installRecorder(t)
	e := holdEngine(auth.RoleWriter)
	first, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.desktopAccess(personContext("ana"), "ana", first.Reference, first.ReasonsDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.lines) != 1 || first.Reference == "" || second.Reference != first.Reference {
		t.Fatalf("lines = %d, references %q and %q, want one line and one reference", len(rec.lines), first.Reference, second.Reference)
	}
}

func TestChangedReasonsWriteANewLineUnderTheSameReference(t *testing.T) {
	rec := installRecorder(t)
	e := holdEngine(auth.RoleWriter)
	first, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.providers.SetClusterAccountGate(func(context.Context, string) bool { return true })
	second, err := e.desktopAccess(personContext("ana"), "ana", first.Reference, first.ReasonsDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.lines) != 2 || rec.lines[1].Reference != first.Reference || second.Reference != first.Reference {
		t.Fatalf("lines = %+v, want a second line under %q", rec.lines, first.Reference)
	}
	if second.ReasonsDigest == first.ReasonsDigest {
		t.Fatalf("digest did not change")
	}
}

func TestAMalformedReferenceIsNeverWritten(t *testing.T) {
	rec := installRecorder(t)
	current, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || current.ReasonsDigest == "" {
		t.Fatalf("first answer = %+v, err = %v", current, err)
	}
	for _, bad := range []string{"ERR-zzzz", "ERR-ABCDEF", "ERR-0123456", "err-abcdef", "x\nERR-abcdef"} {
		rec.lines = nil
		// Even with the CURRENT digest, a value that is not a reference is
		// absent: nothing is returned as-is and a fresh one is written.
		got, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", bad, current.ReasonsDigest)
		if err != nil {
			t.Fatal(err)
		}
		if len(rec.lines) != 1 || rec.lines[0].Reference == bad || !accessReferencePattern.MatchString(rec.lines[0].Reference) || got.Reference != rec.lines[0].Reference {
			t.Fatalf("passed %q: lines = %+v, answer = %+v", bad, rec.lines, got)
		}
	}
}

func TestAnAdmittedAnswerWritesNothing(t *testing.T) {
	rec := installRecorder(t)
	e := engineWithFleet([]FleetModel{accessModel(accessMachine("mine", "ana", true))})
	withAppRows(e)
	e.storedRoleForTest = func(context.Context, string) (auth.Role, bool, error) { return auth.RoleWriter, true, nil }
	got, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || !got.Admitted || got.Exempt || len(got.Sources) != 1 || got.Reference != "" {
		t.Fatalf("answer = %+v, err = %v, want admitted by a source", got, err)
	}
	if len(rec.lines) != 0 {
		t.Fatalf("lines = %+v, want none", rec.lines)
	}
}

func TestACallThatNamesNobodyIsRefused(t *testing.T) {
	rec := installRecorder(t)
	for _, who := range []string{"", "  ", auth.AnonymousUserId} {
		if _, err := holdEngine(auth.RoleWriter).desktopAccess(context.Background(), who, "", ""); err == nil {
			t.Fatalf("%q: want an error", who)
		}
	}
	if len(rec.lines) != 0 {
		t.Fatalf("lines = %+v, want none", rec.lines)
	}
}

func TestAnotherReplicaKeepsThePassedReference(t *testing.T) {
	rec := installRecorder(t)
	first, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", first.Reference, first.ReasonsDigest)
	if err != nil || second.Reference != first.Reference || len(rec.lines) != 1 {
		t.Fatalf("second = %+v, err = %v, lines = %d, want the same reference and no new line", second, err, len(rec.lines))
	}
}

// An error is never a hold (decision 1): the client reads an error as unknown
// and lets the person in.
func TestAFailedReadIsAnErrorNotAHold(t *testing.T) {
	rec := installRecorder(t)
	boom := errors.New("database blinked")

	roleFails := holdEngine(auth.RoleWriter)
	roleFails.storedRoleForTest = func(context.Context, string) (auth.Role, bool, error) { return "", false, boom }
	if got, err := roleFails.desktopAccess(personContext("ana"), "ana", "", ""); err == nil || got.Reference != "" || got.Admitted {
		t.Fatalf("stored role read: answer = %+v, err = %v, want an error and nothing else", got, err)
	}

	sourcesFail := holdEngine(auth.RoleWriter)
	sourcesFail.providers = nil
	if _, err := sourcesFail.desktopAccess(personContext("ana"), "ana", "", ""); err == nil {
		t.Fatalf("sources read: want an error")
	}
	if len(rec.lines) != 0 {
		t.Fatalf("lines = %+v, want none: an error writes no hold line", rec.lines)
	}
}

// Ruling R-D8: a grant or membership read that fails behind the cluster's
// account falls back to the role's answer inside the gate; desktop access
// turns that into an error so a transient failure never holds a person.
func TestAFailedGrantReadBehindTheClusterAccountIsAnError(t *testing.T) {
	rec := installRecorder(t)
	e := holdEngine(auth.RoleWriter)
	e.providers.setEntry(federatedOpenAI(vendorRecord("gpt", VendorOpenAI, true, "")))
	e.providers.SetVendorKeys(noKeys())
	e.providers.SetClusterAccountGate(func(ctx context.Context, _ string) bool {
		auth.NoteReadFailure(ctx, errors.New("grants unreadable"))
		return false
	})
	got, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err == nil || !strings.Contains(err.Error(), "what the person may use could not be read") || !strings.Contains(err.Error(), "grants unreadable") || got.Reference != "" {
		t.Fatalf("answer = %+v, err = %v, want an error", got, err)
	}
	if len(rec.lines) != 0 {
		t.Fatalf("lines = %+v, want none", rec.lines)
	}
}

// Sources are computed for the exempt too, because Ask uses them.
func TestAnExemptPersonStillGetsTheirSources(t *testing.T) {
	installRecorder(t)
	e := engineWithFleet([]FleetModel{accessModel(accessMachine("mine", "ana", true))})
	withAppRows(e)
	e.storedRoleForTest = func(context.Context, string) (auth.Role, bool, error) { return auth.RoleOwner, true, nil }
	got, err := e.desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || !got.Exempt || len(got.Sources) != 1 || got.Sources[0].Kind != AccessSourceMachine {
		t.Fatalf("answer = %+v, err = %v, want exempt with the machine", got, err)
	}
}

// The stored role is read fresh, never from this node's result cache.
func TestTheStoredRoleReadIsMarkedFresh(t *testing.T) {
	installRecorder(t)
	e := holdEngine(auth.RoleWriter)
	fresh := false
	e.storedRoleForTest = func(ctx context.Context, _ string) (auth.Role, bool, error) {
		fresh = FreshReadFromContext(ctx)
		return auth.RoleWriter, true, nil
	}
	if _, err := e.desktopAccess(personContext("ana"), "ana", "", ""); err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("the stored-role read ran on a context not marked fresh")
	}
}

// Ruling R-D9: the digest binds the person, so ana's reference and digest
// carried to ben's check never answer for ben: his digest differs and his own
// line is written.
func TestTheDigestBindsThePerson(t *testing.T) {
	rec := installRecorder(t)
	first, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ben"), "ben", first.Reference, first.ReasonsDigest)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReasonsDigest == first.ReasonsDigest {
		t.Fatalf("both people got digest %q", first.ReasonsDigest)
	}
	if len(rec.lines) != 2 || rec.lines[1].UserId != "ben" {
		t.Fatalf("lines = %+v, want a second line for ben", rec.lines)
	}
}

// A failed write is logged with the person and the error, and answers with no
// reference and no digest.
func TestAFailedHoldWriteIsLoggedAndCarriesNoDigest(t *testing.T) {
	rec := installRecorder(t)
	rec.err = errors.New("log store refused")
	var buf bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prior) })
	got, err := holdEngine(auth.RoleWriter).desktopAccess(personContext("ana"), "ana", "", "")
	if err != nil || got.Reference != "" || got.ReasonsDigest != "" {
		t.Fatalf("answer = %+v, err = %v, want held with no reference and no digest", got, err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "userId=ana") || !strings.Contains(out, "log store refused") {
		t.Fatalf("log = %q, want a warning naming the person and the error", out)
	}
}

// With no database the principal table cannot be asked: that is a failed
// read, never "no such person".
func TestStoredRoleWithNoDatabaseIsAnError(t *testing.T) {
	_, found, err := (&MemQLEngine{}).storedUserRole(context.Background(), "ana")
	if err == nil || found {
		t.Fatalf("found = %v, err = %v, want an error", found, err)
	}
}

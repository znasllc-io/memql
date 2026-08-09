package identity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/metrics"
)

// memql#3381. Ed25519 signing-key rotation is impossible in every deployed
// environment: staging and production supply the key through the sealed env
// envelope, which puts the KeyManager in envMode, where rotation is refused.
// The recorded decision (docs/public/operate/auth/identity-service.md,
// "Rotating the signing key") is to KEEP the manual re-seal-and-roll
// procedure and close the gap with observability rather than to build a
// rotation surface that works in env mode.
//
// These tests pin the two halves of that decision so the next reader is not
// misled the way the dormant scheduler misled: (1) the scheduler really is
// dev-only -- asserted, not assumed; and (2) signing-key age really is
// observable in the env-seed mode that staging and production run.

// --- (1) the scheduler is dev-only ------------------------------------

type recordingAudit struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAudit) Log(_ context.Context, ev AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingAudit) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// TestMaybeRotate_IsInertInEnvSeedMode is the core of the issue. A key far
// older than the rotation interval, in the mode staging and production run,
// must NOT rotate -- and must not emit the {"trigger":"scheduled"} audit
// event that makes rotation look like it happened.
func TestMaybeRotate_IsInertInEnvSeedMode(t *testing.T) {
	// Stamp the seed as minted two years ago: ~8x the 90-day interval, so
	// nothing about the age can excuse the no-op.
	minted := time.Now().UTC().Add(-2 * 365 * 24 * time.Hour)
	km, err := NewKeyManagerFromSeedMintedAt(testSeedB64(), minted)
	if err != nil {
		t.Fatalf("build env-seed KeyManager: %v", err)
	}
	if km.RotationSupported() {
		t.Fatal("env-seed KeyManager reports RotationSupported()==true; staging/prod run this mode and it must be false")
	}

	audit := &recordingAudit{}
	svc := &Service{
		cfg: Config{
			KeyRotationInterval: 90 * 24 * time.Hour,
			JWKSOverlapWindow:   24 * time.Hour,
		},
		logger: quietLogger(),
		keys:   km,
		audit:  audit,
	}

	before := km.Current().KID
	svc.maybeRotate(context.Background())

	if got := km.Current().KID; got != before {
		t.Fatalf("kid changed across maybeRotate in env mode: %q -> %q", before, got)
	}
	if n := audit.count(); n != 0 {
		t.Fatalf("maybeRotate emitted %d audit event(s) in env mode; a scheduled-rotation record here would be a false signal that a deployed cluster rotated", n)
	}
}

// TestMaybeRotate_RotatesOnTheDiskPath is the contrast case: the same
// overdue key on the on-disk dev path DOES rotate, through the JWKS overlap
// window. Without this, the test above could pass against a scheduler that
// is broken everywhere rather than dev-only.
func TestMaybeRotate_RotatesOnTheDiskPath(t *testing.T) {
	km, err := NewKeyManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !km.RotationSupported() {
		t.Fatal("disk-path KeyManager reports RotationSupported()==false")
	}

	// Backdate the freshly minted key past the interval.
	km.mu.Lock()
	km.current.CreatedAt = time.Now().UTC().Add(-100 * 24 * time.Hour)
	km.mu.Unlock()

	audit := &recordingAudit{}
	svc := &Service{
		cfg: Config{
			KeyRotationInterval: 90 * 24 * time.Hour,
			JWKSOverlapWindow:   24 * time.Hour,
		},
		logger: quietLogger(),
		keys:   km,
		audit:  audit,
	}

	before := km.Current().KID
	svc.maybeRotate(context.Background())

	if got := km.Current().KID; got == before {
		t.Fatalf("disk-path maybeRotate did not rotate an overdue key (kid still %q)", got)
	}
	if km.Previous() == nil {
		t.Fatal("disk-path rotation left no previous key; the JWKS overlap window is what keeps in-flight tokens verifying")
	}
	if n := audit.count(); n != 1 {
		t.Fatalf("disk-path rotation emitted %d audit events, want 1", n)
	}
}

// TestRotateNow_RefusedInEnvSeedMode pins the manual path too: there is no
// rotate button anywhere in a deployed cluster, and the error says what to
// do instead.
func TestRotateNow_RefusedInEnvSeedMode(t *testing.T) {
	km, err := NewKeyManagerFromSeed(testSeedB64())
	if err != nil {
		t.Fatalf("build env-seed KeyManager: %v", err)
	}
	svc := &Service{
		cfg:    Config{JWKSOverlapWindow: 24 * time.Hour},
		logger: quietLogger(),
		keys:   km,
		audit:  &recordingAudit{},
	}
	if err := svc.RotateNow(context.Background(), AuditActor{}); err == nil {
		t.Fatal("RotateNow succeeded in env-seed mode; it must refuse")
	}
}

// --- (2) signing-key age is observable --------------------------------

// TestSigningKeyAge_ObservableInEnvSeedMode is the acceptance criterion:
// staging and production run env-seed mode, and the operator must be able to
// see how old the active key is there.
func TestSigningKeyAge_ObservableInEnvSeedMode(t *testing.T) {
	minted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	km, err := NewKeyManagerFromSeedMintedAt(testSeedB64(), minted)
	if err != nil {
		t.Fatalf("build env-seed KeyManager: %v", err)
	}

	got, known := km.CurrentKeyCreatedAt()
	if !known {
		t.Fatal("CurrentKeyCreatedAt reports unknown for a stamped seed")
	}
	if !got.Equal(minted) {
		t.Fatalf("CurrentKeyCreatedAt = %v, want %v", got, minted)
	}

	EmitKeysetMetric(km, time.Now().UTC())
	created, ageKnown, rotationSupported := metrics.IdentitySigningKeyValues()
	if created != float64(minted.Unix()) {
		t.Fatalf("created_timestamp gauge = %v, want %v", created, float64(minted.Unix()))
	}
	if ageKnown != 1 {
		t.Fatalf("age_known gauge = %v, want 1", ageKnown)
	}
	if rotationSupported != 0 {
		t.Fatalf("rotation_supported gauge = %v, want 0 in env-seed mode", rotationSupported)
	}
}

// TestSigningKeyAge_UnstampedSeedReportsUnknownNotZero is the guard against
// re-creating the very shape this issue is about. A raw 32-byte seed carries
// no creation date; the KeyManager fills CreatedAt with time.Now() so the
// struct is well-formed. Publishing THAT as the key's creation date would
// make a three-year-old key report "0 days old" after every pod restart --
// a metric that looks healthiest exactly where rotation never happens.
func TestSigningKeyAge_UnstampedSeedReportsUnknownNotZero(t *testing.T) {
	km, err := NewKeyManagerFromSeed(testSeedB64())
	if err != nil {
		t.Fatalf("build env-seed KeyManager: %v", err)
	}

	if _, known := km.CurrentKeyCreatedAt(); known {
		t.Fatal("CurrentKeyCreatedAt claims to know the creation date of an unstamped seed; it only knows when this process booted")
	}
	// The underlying material still carries a boot-time stamp -- that is
	// what makes the honest-reporting path necessary rather than obvious.
	if km.Current().CreatedAt.IsZero() {
		t.Fatal("expected the KeyMaterial itself to carry a boot-time CreatedAt")
	}

	EmitKeysetMetric(km, time.Now().UTC())
	created, ageKnown, _ := metrics.IdentitySigningKeyValues()
	if ageKnown != 0 {
		t.Fatalf("age_known gauge = %v, want 0 for an unstamped seed", ageKnown)
	}
	if created != 0 {
		t.Fatalf("created_timestamp gauge = %v, want 0 when the date is unknown; a boot-time value here would read as a freshly rotated key", created)
	}
}

// TestSigningKeyAge_KnownOnTheDiskPath: the on-disk envelope persists
// createdAt, so the date survives restarts and needs no operator stamp.
func TestSigningKeyAge_KnownOnTheDiskPath(t *testing.T) {
	km, err := NewKeyManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, known := km.CurrentKeyCreatedAt(); !known {
		t.Fatal("disk-path key reports an unknown creation date")
	}

	EmitKeysetMetric(km, time.Now().UTC())
	_, ageKnown, rotationSupported := metrics.IdentitySigningKeyValues()
	if ageKnown != 1 {
		t.Fatalf("age_known gauge = %v, want 1 on the disk path", ageKnown)
	}
	if rotationSupported != 1 {
		t.Fatalf("rotation_supported gauge = %v, want 1 on the disk path", rotationSupported)
	}
}

// TestSigningKeyCreatedAtConfig covers the env parse, including the
// deliberate choice that a malformed date degrades to "unknown" rather than
// refusing to boot -- it feeds a gauge, not a security control.
func TestSigningKeyCreatedAtConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantSet bool
		want    time.Time
	}{
		{"unset", "", false, time.Time{}},
		{"rfc3339", "2026-03-01T12:00:00Z", true, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"offset normalised to utc", "2026-03-01T13:00:00+01:00", true, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"padded", "  2026-03-01T12:00:00Z  ", true, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
		{"date only is not rfc3339", "2026-03-01", false, time.Time{}},
		{"garbage", "last tuesday", false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT", tc.raw)
			got := envRFC3339("MEMQL_IDENTITY_SIGNING_KEY_CREATED_AT")
			if tc.wantSet != !got.IsZero() {
				t.Fatalf("parsed %q -> %v (zero=%v), wantSet=%v", tc.raw, got, got.IsZero(), tc.wantSet)
			}
			if tc.wantSet && !got.Equal(tc.want) {
				t.Fatalf("parsed %q -> %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

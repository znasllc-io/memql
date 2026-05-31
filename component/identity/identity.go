package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// Service ties the foundation pieces together: config + key manager
// + JWT issuer + audit logger. Subsequent phases extend it with the
// magic-link layer (Phase 2), the web app handlers (Phase 3),
// anti-abuse middleware (Phase 4), the admin app (Phase 6), and the
// PAT layer (Phase 7).
//
// Service is meant to be constructed once at application bootstrap
// (from app/integrations_identity.go under the `identity` build tag)
// and lives for the process lifetime. RegisterRoutes is called once
// to mount its handlers on a router; Start kicks off background
// rotation; Shutdown stops the background work and is idempotent.
type Service struct {
	cfg    Config
	logger *slog.Logger

	keys   *KeyManager
	issuer *JWTIssuer
	audit  AuditLogger

	// Phase 2: magic-link / refresh / token / logout HTTP routes.
	// Wired by app/integrations_identity.go after the engine is up,
	// since the underlying Store needs an EngineExecutor. Stored as
	// HTTPMounter (interface) so the Service file does not import the
	// http subpackage and create a cycle with subpackages that
	// already import this one.
	httpMounter HTTPMounter

	// Phase 3: web UI mounter — login form, check-email, landing,
	// logout splash, error page, /setup wizard, /legal/* docs, and
	// /me/* shells. Same interface shape as httpMounter.
	webMounter HTTPMounter

	// Phase 6: admin web app mounter — /admin/* dashboard, users,
	// audit, JWKS, settings. Mounted on the same mux as the web/HTTP
	// surfaces. Auth-gated inside the admin handler (role=owner|admin).
	adminMounter HTTPMounter

	// Phase 7: PAT verifier — accepts either an mql_pat_<...> Personal
	// Access Token or a JWT, returns unified claims. Wired here so the
	// gRPC interceptor can call svc.VerifyToken(ctx, raw) when the
	// cluster's binaries (bff/voice/etc.) switch to identity-issued
	// tokens in Phase 8. Stored as a typed *any so the identity package
	// doesn't import the pat subpackage (which would import identity).
	patVerifier any
	patStore    any

	// Background rotation goroutine state.
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// HTTPMounter is the narrow interface the identity Service uses to
// delegate HTTP route mounting to a subpackage handler. The
// component/identity/http package's *Server satisfies this directly.
//
// Splitting the interface out here lets Service hold a typed
// reference without importing the subpackage (which already imports
// component/identity).
type HTTPMounter interface {
	Mount(mux *http.ServeMux)
}

// NewService builds a Service from a validated Config. The caller is
// expected to have already called Config.Validate(); errors here are
// limited to filesystem / cryptographic failures.
func NewService(cfg Config, logger *slog.Logger, audit AuditLogger) (*Service, error) {
	if logger == nil {
		return nil, errors.New("identity: logger required")
	}
	if !cfg.Enabled {
		return nil, errors.New("identity: NewService called with cfg.Enabled=false")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("identity: config validation failed: %w", err)
	}

	// #550: when a signing key is provided via the sealed envelope
	// (IDENTITY_SIGNING_KEY_B64), every replica derives the same key from
	// it -- no ReadWriteOnce key PVC, so identity can run >=2 replicas on
	// RollingUpdate. Otherwise fall back to the on-disk KeyDir (dev).
	var (
		km  *KeyManager
		err error
	)
	if cfg.SigningKeyB64 != "" {
		km, err = NewKeyManagerFromSeed(cfg.SigningKeyB64)
	} else {
		km, err = NewKeyManager(cfg.KeyDir, cfg.KeyEncryptionKey)
	}
	if err != nil {
		return nil, err
	}
	if err := km.Load(); err != nil {
		return nil, fmt.Errorf("identity: load keys: %w", err)
	}

	iss, err := NewJWTIssuer(km, cfg)
	if err != nil {
		return nil, fmt.Errorf("identity: build JWT issuer: %w", err)
	}

	if audit == nil {
		audit = &SlogAuditLogger{Logger: logger, DB: NoopAuditDBSink{}}
	}

	return &Service{
		cfg:    cfg,
		logger: logger.With(slog.String("component", ComponentName)),
		keys:   km,
		issuer: iss,
		audit:  audit,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}, nil
}

// Config returns the snapshot the service was built with.
func (s *Service) Config() Config { return s.cfg }

// Keys exposes the key manager. Used by the JWKS handler and by
// rotation triggers.
func (s *Service) Keys() *KeyManager { return s.keys }

// Issuer exposes the JWT issuer for callsites that mint or verify
// tokens (refresh handlers, admin actions, etc., in later phases).
func (s *Service) Issuer() *JWTIssuer { return s.issuer }

// Audit returns the audit logger.
func (s *Service) Audit() AuditLogger { return s.audit }

// RegisterRoutes mounts the handlers this Service is responsible for
// on the supplied mux. Phase 1 only mounts the JWKS endpoint;
// later phases append the auth, OAuth, admin, and `/me` routes.
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	if mux == nil {
		return
	}
	mux.Handle("GET /.well-known/jwks.json", JWKSHandler(s.keys))
	// Discovery document for `memql-cockpit authorize <url>`. Tells
	// the cockpit which gRPC endpoint to dial + which OAuth
	// client_id to use, so the user only has to paste the URL.
	mux.Handle("GET /.well-known/memql-config.json", DiscoveryHandler(s.cfg, os.Getenv))
	if s.httpMounter != nil {
		s.httpMounter.Mount(mux)
	}
	if s.webMounter != nil {
		s.webMounter.Mount(mux)
	}
	if s.adminMounter != nil {
		s.adminMounter.Mount(mux)
	}
	s.logger.Info("registered identity routes",
		slog.String("base_url", s.cfg.BaseURL),
		slog.String("registered_clients", clientIds(s.cfg.RegisteredClients)),
	)
}

// SetHTTPMounter wires a Phase-2-or-later HTTP mounter onto the
// Service. Called once at bootstrap, before RegisterRoutes runs.
// Idempotent — last value wins.
func (s *Service) SetHTTPMounter(m HTTPMounter) {
	if s == nil {
		return
	}
	s.httpMounter = m
}

// SetWebMounter wires the Phase-3 web UI mounter onto the Service.
// Idempotent — last value wins.
func (s *Service) SetWebMounter(m HTTPMounter) {
	if s == nil {
		return
	}
	s.webMounter = m
}

// SetAdminMounter wires the Phase-6 admin UI mounter onto the
// Service. Mounts on the same mux as the public web pages but under
// the /admin/ prefix; the admin package enforces its own
// role=owner|admin auth gate. Idempotent — last value wins.
func (s *Service) SetAdminMounter(m HTTPMounter) {
	if s == nil {
		return
	}
	s.adminMounter = m
}

// SetPATVerifier stashes the *pat.Verifier the wiring layer constructs
// in app/integrations_identity.go. Held as `any` so this file doesn't
// import the pat subpackage (which already imports identity). Callers
// that need to verify tokens type-assert back to the concrete type or
// wrap the read with a small helper. Idempotent — last value wins.
func (s *Service) SetPATVerifier(v any) {
	if s == nil {
		return
	}
	s.patVerifier = v
}

// PATVerifier returns whatever was set via SetPATVerifier, or nil.
// Callers (typically the gRPC interceptor in Phase 8) cast to
// *pat.Verifier and call VerifyToken.
func (s *Service) PATVerifier() any {
	if s == nil {
		return nil
	}
	return s.patVerifier
}

// SetPATStore stashes the *pat.Store the wiring layer constructs.
// Same any-shaped indirection as SetPATVerifier so identity.go avoids
// importing the pat subpackage. Idempotent.
func (s *Service) SetPATStore(st any) {
	if s == nil {
		return
	}
	s.patStore = st
}

// PATStore returns whatever was set via SetPATStore.
func (s *Service) PATStore() any {
	if s == nil {
		return nil
	}
	return s.patStore
}

// Start launches background goroutines (currently: the key-rotation
// loop). Idempotent — only the first call has an effect.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		go s.rotationLoop(ctx)
	})
}

// Shutdown stops background goroutines and waits up to a few seconds
// for them to exit. Safe to call multiple times.
func (s *Service) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	select {
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("identity: rotation loop did not exit within 5s")
	}
}

// rotationLoop drives the 90-day automatic key rotation and the
// 24-hour previous-key sweep. Wakes hourly to check both deadlines —
// the cost is negligible and it keeps the loop simple.
func (s *Service) rotationLoop(parent context.Context) {
	defer close(s.doneCh)
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-parent.Done():
			return
		case <-tick.C:
			s.maybeRotate(parent)
			s.maybeSweep(parent)
		}
	}
}

// maybeRotate checks the current key's age and rotates if older than
// the configured interval.
func (s *Service) maybeRotate(ctx context.Context) {
	// #550: an env-provided signing key can't be auto-rotated (it's
	// sealed in the envelope + shared across replicas); rotate by
	// re-sealing + rolling instead.
	if !s.keys.RotationSupported() {
		return
	}
	cur := s.keys.Current()
	if cur == nil {
		return
	}
	if s.cfg.KeyRotationInterval <= 0 {
		return
	}
	if time.Since(cur.CreatedAt) < s.cfg.KeyRotationInterval {
		return
	}
	if _, err := s.keys.Rotate(s.cfg.JWKSOverlapWindow); err != nil {
		s.logger.Error("key_rotation_failed", slog.String("error", err.Error()))
		return
	}
	s.audit.Log(ctx, AuditEvent{
		Category: AuditCategoryConfiguration,
		Action:   "jwks_rotated",
		Detail:   map[string]any{"trigger": "scheduled", "intervalDays": int(s.cfg.KeyRotationInterval / (24 * time.Hour))},
		Outcome:  AuditOutcomeSuccess,
	})
	s.logger.Info("jwt_signing_key_rotated", slog.String("kid", s.keys.Current().KID))
}

// maybeSweep removes the previous key from JWKS once its retirement
// deadline has passed.
func (s *Service) maybeSweep(ctx context.Context) {
	swept, err := s.keys.SweepRetiredPrevious(time.Now().UTC())
	if err != nil {
		s.logger.Error("key_sweep_failed", slog.String("error", err.Error()))
		return
	}
	if swept {
		s.logger.Info("retired_jwt_key_removed_from_jwks")
		s.audit.Log(ctx, AuditEvent{
			Category: AuditCategoryConfiguration,
			Action:   "jwks_previous_key_retired",
			Outcome:  AuditOutcomeSuccess,
		})
	}
}

// RotateNow forces an immediate key rotation. Wired from the admin
// UI's "Rotate now" button (Phase 6) and useful for tests.
func (s *Service) RotateNow(ctx context.Context, actor AuditActor) error {
	if _, err := s.keys.Rotate(s.cfg.JWKSOverlapWindow); err != nil {
		return err
	}
	s.audit.Log(ctx, AuditEvent{
		Category:    AuditCategoryConfiguration,
		Action:      "jwks_rotated",
		ActorUserId: actor.UserId,
		ActorEmail:  actor.Email,
		ActorRole:   actor.Role,
		Detail:      map[string]any{"trigger": "manual"},
		Outcome:     AuditOutcomeSuccess,
	})
	return nil
}

// AuditActor is a small struct callers pass when emitting audit
// events to attribute the action.
type AuditActor struct {
	UserId     string
	Email      string
	Role       string
	IdentityId string
}

// clientIds returns a human-readable summary of registered clients
// for log lines.
func clientIds(clients []RegisteredClient) string {
	if len(clients) == 0 {
		return "(none)"
	}
	out := ""
	for i, c := range clients {
		if i > 0 {
			out += ","
		}
		out += c.ClientId
	}
	return out
}

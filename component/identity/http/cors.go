package http

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// grantedOriginTTL is how long the middleware may reuse the granted-origin set
// it last read.
//
// NOT a latency optimisation, and deliberately not an env var. cors() wraps ~15
// routes -- including every unauthenticated OPTIONS preflight, POST /register
// and the WebAuthn login pair -- so without a window an anonymous flood of
// unknown origins is one database query per request against the auth surface.
// Ten seconds bounds that to one query per interval while keeping the property
// the whole change exists for: a grant takes effect with no restart and no
// operator action, just not in the same millisecond. A revoke lands inside the
// same window.
//
// A constant rather than a knob because a knob would oblige an entry in
// scripts/secrets/manifest.yaml, its embedded mirror and the env-registry gate
// for a value nobody has asked to tune.
const grantedOriginTTL = 10 * time.Second

// grantedOriginReadTimeout bounds ONE granted-origin read.
//
// It exists because of the mutex, not because of the query. `current` holds the
// lock across the read (deliberately -- see there), and `sync.Mutex.Lock` is NOT
// context-aware: a waiter cannot be cancelled by its own client hanging up. So
// the only thing that drains a queue of parked preflights is the HOLDER
// finishing, and on an unauthenticated surface an unbounded holder means
// unbounded goroutine accumulation during a database outage.
//
// Nothing else on this path supplies a deadline. `r.Context()` arrives without
// one: component/server's 15s read / 15s write / 60s idle are connection-level
// and do not cancel a handler's context, and no http.TimeoutHandler wraps these
// routes. What actually terminated the hang case before this constant existed
// was bun's pgdriver socket deadline -- `ReadTimeout`, defaulted to 10s in
// pgdriver/config.go -- which is a DEPENDENCY'S DEFAULT rather than anything this
// package states. Three seconds sits well under it, so the bound is ours and
// stays ours if that default changes. It is also far longer than a healthy read
// of a table holding at most a handful of granted rows: a read that misses this
// deadline is an outage, and the outage answer is the fail-closed one below.
const grantedOriginReadTimeout = 3 * time.Second

// grantedOrigins is the short window over the admin-granted half of identity's
// CORS allowlist (memql#3716).
//
// # WHY A LIVE READ AND NOT A CDC-INVALIDATED CACHE
//
// The obvious design is a long-lived cache invalidated on the concept's change
// feed. It cannot work here, for two measured reasons:
//
//   - The write can land on a DIFFERENT NODE than this middleware. The
//     IdentityAdminMsg bridge is wired on every node with an engine
//     (app/transport.go), deliberately -- so an admin's grant may execute on
//     `bff` while cors() runs on `identity`. In-process invalidation cannot
//     help.
//   - The identity node is filtered OUT of every mesh broadcast target list
//     (component/node/eventbridge.go's meshEventParticipants, added by
//     memql#3380 when the bff started dialing identity for deploy control), so
//     a cross-node invalidation could not reach this node either.
//
// A CDC-invalidated cache would pass a single-node test and silently fail in the
// 2-replica mesh -- the bug class multi-node-by-default exists to prevent. So
// correctness lives in the read being live; the window is only a rate bound, and
// removing it would slow things down without breaking them.
//
// What makes the live read cross-node-correct with no plumbing at all is that
// every node shares ONE PostgreSQL database. The grant is a row, not an event:
// whichever node performed the write, the node making the CORS decision sees it
// on its next read. That is the property a cache would have thrown away.
//
// The pattern is the one already working next door:
// component/identity/admin.LiveSettingsReader runs clusterSettingsCurrent on
// every call and falls back to boot config, which is what makes
// registeredClientsJSON admin-editable without a restart.
type grantedOrigins struct {
	// read fetches the current granted set. Nil means "no graph", which yields
	// no granted origins and no error -- a binary wired without a store has the
	// env list as its whole allowlist, which is correct for it.
	read func(context.Context) ([]string, error)
	// log receives one line per failed read. Optional.
	log *slog.Logger
	// ttl bounds re-reading. ZERO means "read on every call", which is what the
	// tests set so a grant is observable without sleeping through the
	// production window.
	ttl time.Duration
	// now is the window's clock. Nil means time.Now. A seam rather than a clock
	// abstraction: one field, set by one test.
	now func() time.Time

	mu     sync.Mutex
	cached []string
	readAt time.Time
	valid  bool
}

// current returns the granted origins in effect for this request.
//
// The read happens with the lock HELD, which serialises concurrent misses into
// one query rather than letting a window boundary become a stampede across every
// in-flight preflight. That is the right trade on this surface: at most one
// query is in flight, and the alternative (double-checked locking) turns an
// expiry under load into N simultaneous reads.
//
// What makes that safe is the DEADLINE, and it is easy to remove by accident.
// `sync.Mutex.Lock` is not context-aware, so a waiter parks until the holder
// finishes no matter what its own client does -- the holder is therefore bounded
// explicitly by grantedOriginReadTimeout rather than left to bun's pgdriver
// socket deadline (`ReadTimeout`, 10s by default), which is where that bound
// silently lived before. A later refactor narrowing the lock should know that is
// what it is trading away.
func (g *grantedOrigins) current(ctx context.Context) []string {
	if g == nil || g.read == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	clock := time.Now
	if g.now != nil {
		clock = g.now
	}
	if g.valid && g.ttl > 0 && clock().Sub(g.readAt) < g.ttl {
		return g.cached
	}

	// Derived from the REQUEST context, so a client that hangs up still cancels
	// the read it is holding -- the deadline is the backstop for the waiters
	// behind it, not a replacement for cancellation.
	readCtx, cancel := context.WithTimeout(ctx, grantedOriginReadTimeout)
	defer cancel()
	origins, err := g.read(readCtx)
	if err != nil {
		// FAIL CLOSED, and drop what we were holding rather than serving it.
		//
		// Refusing an origin is recoverable: the browser's next preflight is a
		// fresh attempt and succeeds the moment the graph is reachable, because
		// a failed read does not start a window. Serving a set we can no longer
		// verify is not recoverable in the direction that matters -- it would
		// let a database outage outlive a REVOKE, and revoking is half of what
		// this surface exists to do.
		//
		// The env list is untouched by any of this, so identity keeps serving
		// its own login page, the portal and the app through the outage.
		g.cached, g.valid = nil, false
		if g.log != nil {
			g.log.Warn("identity CORS: granted-origin read failed; only the boot-time "+
				"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS list is in effect for now",
				slog.String("error", err.Error()))
		}
		return nil
	}
	g.cached, g.valid, g.readAt = origins, true, clock()
	return origins
}

// corsGrantReader returns this Server's granted-origin window, building it on
// first use.
//
// Lazily built and Server-scoped rather than a package global for the same
// reason badgeGrantLimiter is: each Server (and each test) gets its own state
// and captures its own logger. A test that installs its own reader before the
// first request keeps it.
func (s *Server) corsGrantReader() *grantedOrigins {
	s.corsGrantsOnce.Do(func() {
		if s.corsGrants == nil {
			s.corsGrants = &grantedOrigins{
				read: s.readGrantedCORSOrigins,
				log:  s.Logger,
				ttl:  grantedOriginTTL,
			}
		}
	})
	return s.corsGrants
}

// readGrantedCORSOrigins is the graph half of the allowlist: every origin an
// owner/admin has granted on a v1:identity:oauthClient row.
func (s *Server) readGrantedCORSOrigins(ctx context.Context) ([]string, error) {
	// Engine is checked as well as Store, and not as belt-and-braces: the store's
	// executeAndExtract dereferences s.Engine unguarded, and the package's
	// convention of half-guarding a Store was survivable while every caller was
	// an authenticated handler. This is the first time the CORS path touches the
	// store, so a nil engine would now panic on every OPTIONS preflight and on
	// POST /register -- unauthenticated routes, no credential required to reach
	// them.
	if s == nil || s.Store == nil || s.Store.Engine == nil {
		// No graph to read. Not an error: the env list is the whole allowlist for
		// such a binary, which is the pre-memql#3716 behaviour and correct for it.
		return nil, nil
	}
	return s.Store.GrantedCORSOrigins(ctx)
}

// cors wraps a handler with CORS headers.
//
// A matched origin gets Access-Control-Allow-Credentials: true, so an entry on
// this allowlist is permission to make cookie-bearing requests to identity's
// auth endpoints AND READ THE RESPONSES. Treat every change here as a change to
// what can reach a session.
//
// The allowlist has TWO sources (memql#3716):
//
//  1. the boot-time MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS list on Cfg -- the
//     bootstrap set every deployment has (identity itself, the portal, the app),
//     derived from MEMQL_DOMAIN in component/envregistry/domain.go;
//  2. origins an owner/admin GRANTED on a v1:identity:oauthClient row, read live
//     per request so a grant needs no identity restart.
//
// Resolution happens per REQUEST, not at wrap-registration time. The captured
// `allowed := s.Cfg.CORSAllowedOrigins` this replaced was the whole defect: a
// fixed set, snapshotted once, where an open set belongs -- so admitting one new
// customer's website cost an env change and a restart.
func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowedForRequest(r.Context(), origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		next(w, r)
	}
}

// originAllowedForRequest resolves both sources for one request.
//
// The env list is checked FIRST and in memory. That ordering is deliberate: it
// covers identity, the portal and the app -- effectively all real traffic -- so
// the common case never touches the graph, and the origins a cluster needs to
// serve its own login page cannot be made to depend on a database being up.
// Only a miss consults the granted rows.
func (s *Server) originAllowedForRequest(ctx context.Context, origin string) bool {
	if originAllowed(s.Cfg.CORSAllowedOrigins, origin) {
		return true
	}
	return originAllowed(s.corsGrantReader().current(ctx), origin)
}

// handleOptions short-circuits CORS preflight. The cors() wrapper has
// already attached the headers; we just need to return a 204.
func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// originAllowed returns true when origin matches an entry in allowed. Exact
// match, case-insensitive, surrounding whitespace ignored -- the env list is a
// comma-separated string an operator typed.
//
// "*" IS NOT HONOURED, from either source. The comment that used to sit here
// said the wildcard was "only ever used for non-credentialed requests, but we
// treat it as a permissive override" -- describing a property this code does not
// have. cors() sets Access-Control-Allow-Credentials: true unconditionally on
// every match, so a single "*" entry granted credentialed cross-origin read
// access to every origin on the internet, and would make the owner/admin grant
// this file resolves decorative (memql#3716).
//
// Measured safe to refuse: nothing in the tree sets it. The only assignments are
// deploy/k8s/base/identity.yaml, the local overlay's
// MEMQL_IDENTITY_CORS_EXTRA_ORIGINS, and the derivation in
// component/envregistry/domain.go -- all explicit origin lists. A "*" that somehow
// reaches here is skipped rather than rejected loudly, so the explicit entries
// beside it keep working.
func originAllowed(allowed []string, origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" || a == "*" {
			continue
		}
		if strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

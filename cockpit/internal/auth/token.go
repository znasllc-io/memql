package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// EnsureValidToken loads a cached token for the cluster, checks expiry,
// and triggers re-authentication if needed. Returns a valid access token.
//
// Resolution priority:
//  1. cluster.PAT      -> the literal PAT string (no expiry; revocation
//     is server-side via /me/tokens or /admin/tokens)
//  2. cached + fresh   -> return cached access token
//  3. cached + expired -> if a refresh token is cached, hit
//     /auth/refresh and roll the pair forward.
//     Success = silent, no browser. invalid_grant
//     (refresh expired/revoked) falls through to (4).
//     Transient errors (5xx, network) also fall
//     through but DO NOT delete the cached token,
//     so a later retry can succeed silently.
//  4. fresh login      -> open browser, mint a new pair, cache, return.
//
// Every path goes through the identity service. There is NO
// no-auth shortcut -- the dev environment must run the identity
// service like production does, the cockpit must authorize against
// it like any other cluster, and any code path that wants to skip
// auth has to deal with that locally and not via the CLI surface.
//
// The `logger` is consulted only for non-fatal events on the refresh
// path (refresh attempted, rolled, fell through). Pass nil to silence.
func EnsureValidToken(ctx context.Context, cluster config.ClusterConfig) (string, error) {
	return ensureValidToken(ctx, cluster, nil)
}

// EnsureValidTokenWithLogger is the logger-aware variant. Used by the
// pool's lifecycle (which has a slog handler at hand) so refresh
// activity surfaces in the same log stream as dial / connect events.
func EnsureValidTokenWithLogger(ctx context.Context, cluster config.ClusterConfig, logger *slog.Logger) (string, error) {
	return ensureValidToken(ctx, cluster, logger)
}

// interactiveLogin is the browser / device sign-in EnsureValidToken falls
// back to. A variable only so a test can stand in for the browser and
// see which client the sign-in asked for.
var interactiveLogin = InteractiveLogin

// ErrLoginRequired is what the non-interactive path returns where the
// interactive one would have opened a browser.
var ErrLoginRequired = errors.New("this cluster needs a sign-in that cannot be done from here")

// EnsureValidTokenNonInteractive is EnsureValidToken for callers that must
// never open a browser (memql#4841).
//
// THE BACKGROUND WORKER IS THE CASE. `memql worker run` is started by a
// LaunchAgent or a systemd unit, usually with no session, no display and
// nobody watching -- so the browser step is not "slow", it is a process that
// blocks forever on a window that will never be opened, on a machine whose
// owner is asleep. main.go already refuses to install the Keychain credential
// store on the worker path for the same reason, and this is the other half of
// that decision.
//
// Every silent path is unchanged: a PAT short-circuits, a cached token is
// returned, and an expired one still rolls forward through /auth/refresh. Only
// the last resort differs -- it returns ErrLoginRequired, so a caller can say
// "run `memql login` on this machine" instead of hanging.
func EnsureValidTokenNonInteractive(ctx context.Context, cluster config.ClusterConfig, logger *slog.Logger) (string, error) {
	return ensureValidTokenMode(ctx, cluster, logger, false)
}

func ensureValidToken(ctx context.Context, cluster config.ClusterConfig, logger *slog.Logger) (string, error) {
	return ensureValidTokenMode(ctx, cluster, logger, true)
}

func ensureValidTokenMode(ctx context.Context, cluster config.ClusterConfig, logger *slog.Logger, interactive bool) (string, error) {
	// PAT path -- short-circuit the OIDC dance entirely. The PAT IS
	// the bearer token; the server unwraps it via the pat package's
	// Verifier (component/identity/pat/verifier.go).
	if cluster.PAT != "" {
		return cluster.PAT, nil
	}

	// An absent client_id is the normal case, not a misconfiguration: the
	// cockpit does not store its own default client in the shared
	// registry, and EffectiveClientId supplies it.
	if cluster.Issuer == "" {
		return "", fmt.Errorf("cluster %q has no issuer / PAT configured. Re-run `memql cluster add <domain>` to register it against an identity service", cluster.Name)
	}

	stored, err := config.LoadToken(cluster.Name)
	if err != nil {
		return "", fmt.Errorf("load cached token: %w", err)
	}

	if stored != nil && !stored.IsExpired() {
		return stored.AccessToken, nil
	}

	// Cached token has expired (or is within the IsExpired buffer).
	// Try the silent refresh path before opening a browser. This is
	// the entire point of the refresh-token grant: the user should
	// never see a re-auth prompt as long as the refresh token is
	// still valid server-side.
	if stored != nil && stored.RefreshToken != "" {
		if logger != nil {
			logger.Info("auth: refreshing access token", "cluster", cluster.Name)
		}
		result, refreshErr := Refresh(ctx, cluster.Issuer, stored.RefreshToken)
		if refreshErr == nil {
			if err := config.SaveToken(cluster.Name, &config.StoredToken{
				AccessToken:  result.AccessToken,
				RefreshToken: result.RefreshToken,
				Expiry:       result.Expiry,
			}); err != nil && logger != nil {
				logger.Warn("auth: token refreshed but cache write failed", "cluster", cluster.Name, "error", err)
			}
			return result.AccessToken, nil
		}
		// Distinguish "session is dead, must re-consent" from
		// "transient failure, try again later". The former is the
		// ONLY case where we open a browser; the latter surfaces an
		// error so the pool's reconnect loop backs off instead of
		// flashing a re-auth prompt on every retry.
		if errors.Is(refreshErr, ErrInvalidGrant) {
			if logger != nil {
				logger.Info("auth: refresh token rejected, prompting for re-login", "cluster", cluster.Name, "error", refreshErr)
			}
			// Fall through to the browser flow.
		} else {
			if logger != nil {
				logger.Warn("auth: refresh failed (transient), retaining cached refresh token", "cluster", cluster.Name, "error", refreshErr)
			}
			return "", fmt.Errorf("refresh access token: %w", refreshErr)
		}
	}

	// No refresh token, or refresh terminally rejected. Open the
	// browser and ask for a fresh login -- unless the caller cannot have
	// one, in which case say so rather than blocking on a window nobody
	// will ever see.
	if !interactive {
		return "", fmt.Errorf("%w: run `memql login` on this machine (cluster %q)", ErrLoginRequired, cluster.Name)
	}
	result, err := interactiveLogin(ctx, cluster.Issuer, cluster.EffectiveClientId())
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}

	if err := config.SaveToken(cluster.Name, &config.StoredToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		Expiry:       result.Expiry,
	}); err != nil && logger != nil {
		// Non-fatal: token works but won't be cached.
		logger.Warn("auth: login succeeded but cache write failed", "cluster", cluster.Name, "error", err)
	}

	return result.AccessToken, nil
}

// Logout removes cached credentials for a cluster.
func Logout(clusterName string) error {
	return config.DeleteToken(clusterName)
}

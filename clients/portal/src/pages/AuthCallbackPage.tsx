import { useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { isRuntimeConfigReady } from "../cluster/config";
import { ErrorMessage } from "../components/StatusMessage";
import { Skeleton } from "../ui";
import { DEFAULT_RETURN_TO } from "../auth/pending";

// Where the identity service sends the browser back to, carrying either
// ?code=&state= or ?error=&error_description=.
//
// Three jobs, in order:
//   1. Exchange the code for an access token (PKCE-bound; see identityClient).
//   2. SCRUB the query string out of the address bar.
//   3. Send the operator to the route they originally asked for.
//
// ---------------------------------------------------------------------------
// ON THE SCRUB, and why it is not security theatre
// ---------------------------------------------------------------------------
// The authorization code is not a credential an attacker can use on its own:
// it is single-use, minutes-lived, and bound by PKCE to a verifier that never
// left this browser. It is nonetheless removed from the URL immediately,
// because a URL is the most casually-copied string in a browser -- into a bug
// report, a screenshot, a shared link, a `document.referrer` sent to the next
// origin. Leaving a spent code there gains nothing and invites all of that.
// history.replaceState (not pushState) so "back" does not land on a callback
// whose code has already been consumed, which would render an error page for
// an action that actually succeeded.

export function AuthCallbackPage(): ReactNode {
  const { completeSignIn, config, status, error: sessionError } = useAuth();
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [error, setError] = useState("");

  // The exchange must run EXACTLY once. The authorization code is single-use
  // -- identity audits a second presentation as `auth_code_replay` and refuses
  // it -- and React 19's StrictMode runs effects twice in development, which
  // would turn every local sign-in into a replay-rejected failure.
  const started = useRef(false);

  useEffect(() => {
    if (started.current) return;

    const code = params.get("code") ?? "";
    const state = params.get("state") ?? "";
    const oauthError = params.get("error") ?? "";

    // Scrub first, before any await. The window between arriving and
    // exchanging is where a screenshot or an accidental copy happens, and it
    // costs nothing to close it immediately. useSearchParams keeps the
    // router's copy; history.replaceState does not clear that.
    if (code || state || oauthError) {
      scrubQueryString();
    }

    if (oauthError) {
      // identity redirects failures back here rather than dead-ending on its
      // own error page, so this is a normal path, not an exceptional one.
      started.current = true;
      setError(
        params.get("error_description") ||
          `The identity service refused the sign-in (${oauthError}).`,
      );
      return;
    }
    if (!code || !state) {
      started.current = true;
      setError("The sign-in response was missing its authorization code.");
      return;
    }

    // This page sits outside RequireAuth, so it paints on the first tick of
    // a cold load while AuthProvider is still reading runtime-config.json.
    // completeSignIn reads configRef.current; if that is still
    // UNKNOWN_RUNTIME_CONFIG, oauthClientId is empty and identity answers
    // "code, client_id, and redirect_uri are required" (memql#4154). Wait.
    // Do not set started until we actually hand the code over -- otherwise
    // a late-arriving config can never retry.
    if (!isRuntimeConfigReady(config)) {
      if (status === "misconfigured") {
        started.current = true;
        setError(
          sessionError ||
            "This cluster is not configured for portal sign-in. See docs/public/operate/portal.md.",
        );
      }
      return;
    }

    started.current = true;

    void completeSignIn({ code, state }).then((result) => {
      if (result.ok) {
        navigate(result.returnTo || DEFAULT_RETURN_TO, { replace: true });
        return;
      }
      setError(result.error);
    });
  }, [completeSignIn, navigate, params, config, status, sessionError]);

  return (
    <div className="flex min-h-full items-center justify-center bg-bg p-6 text-fg">
      <div className="w-full max-w-md rounded-lg border border-line bg-surface p-6">
        <h1 className="text-lg font-semibold tracking-tight">Signing in</h1>
        {error ? (
          <div className="mt-4">
            <ErrorMessage>{error}</ErrorMessage>
            <button
              type="button"
              onClick={() => navigate(DEFAULT_RETURN_TO, { replace: true })}
              className="mt-4 rounded border border-line px-3 py-1.5 text-sm hover:bg-raised"
            >
              Back to the portal
            </button>
          </div>
        ) : (
          <div className="mt-3">
            <Skeleton variant="text" width="w-40" />
          </div>
        )}
      </div>
    </div>
  );
}

// scrubQueryString rewrites the address bar to this path with no query.
// Guarded because history is absent under some test renderers, and a missing
// History API must not break the sign-in it is only cosmetically improving.
function scrubQueryString(): void {
  const history = globalThis.history;
  const location = globalThis.location;
  if (!history?.replaceState || !location) return;
  try {
    history.replaceState(history.state, "", location.pathname);
  } catch {
    // Non-fatal.
  }
}

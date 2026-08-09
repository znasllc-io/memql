import { useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { ErrorMessage, Loading } from "../components/StatusMessage";
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
  const { completeSignIn } = useAuth();
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
    started.current = true;

    const code = params.get("code") ?? "";
    const state = params.get("state") ?? "";
    const oauthError = params.get("error") ?? "";

    // Scrub first, before any await. The window between arriving and
    // exchanging is where a screenshot or an accidental copy happens, and it
    // costs nothing to close it immediately.
    scrubQueryString();

    if (oauthError) {
      // identity redirects failures back here rather than dead-ending on its
      // own error page, so this is a normal path, not an exceptional one.
      setError(
        params.get("error_description") ||
          `The identity service refused the sign-in (${oauthError}).`,
      );
      return;
    }
    if (!code || !state) {
      setError("The sign-in response was missing its authorization code.");
      return;
    }

    void completeSignIn({ code, state }).then((result) => {
      if (result.ok) {
        navigate(result.returnTo || DEFAULT_RETURN_TO, { replace: true });
        return;
      }
      setError(result.error);
    });
  }, [completeSignIn, navigate, params]);

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
            <Loading what="your session" />
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

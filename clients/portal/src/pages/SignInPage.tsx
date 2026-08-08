import { useLocation } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { clusterLabelFor } from "../cluster/endpoint";
import { ErrorMessage } from "../components/StatusMessage";

// The sign-in view.
//
// Rendered IN PLACE of the requested route rather than at a /signin URL, on
// purpose: the address bar keeps showing the deep link the operator asked for,
// so the URL they eventually bookmark or share is the one they wanted, and a
// browser refresh mid-sign-in returns to the same place. (The path is also
// persisted across the redirect to identity -- see auth/pending.ts -- because
// the document itself is destroyed by that navigation.)
//
// The cluster is named here, before sign-in, because it is the one thing the
// operator must confirm BEFORE authenticating: "am I about to sign in to
// production?" is a question worth answering while it is still cheap.

export function SignInPage(): ReactNode {
  const { signIn, error, config, status } = useAuth();
  const location = useLocation();
  const cluster = clusterLabelFor(globalThis.location);
  const returnTo = location.pathname + location.search;

  return (
    <div className="flex min-h-full items-center justify-center bg-bg p-6 text-fg">
      <div className="w-full max-w-md rounded-lg border border-line bg-surface p-6">
        <h1 className="text-lg font-semibold tracking-tight">memQL Portal</h1>
        <p className="mt-1 text-sm text-muted">
          Sign in to{" "}
          <span className="font-mono text-fg">{cluster || "this cluster"}</span>.
        </p>

        {status === "misconfigured" ? (
          <div className="mt-4">
            <ErrorMessage>
              {error ||
                "This cluster is not configured for portal sign-in. See docs/public/operate/portal.md."}
            </ErrorMessage>
          </div>
        ) : (
          <>
            <button
              type="button"
              onClick={() => signIn(returnTo)}
              className="mt-5 w-full rounded bg-accent px-3 py-2 text-sm font-medium text-accent-fg hover:opacity-90"
            >
              Continue with memQL identity
            </button>
            <p className="mt-3 text-xs text-subtle">
              You will be sent to{" "}
              <span className="font-mono">{hostOf(config.identityUrl)}</span> to
              sign in with a magic link, then returned here.
            </p>
            {error ? (
              <div className="mt-4">
                <ErrorMessage>{error}</ErrorMessage>
              </div>
            ) : null}
          </>
        )}
      </div>
    </div>
  );
}

// hostOf renders just the host of the identity URL. Showing the operator WHERE
// they are about to be sent is the whole anti-phishing value of this line, and
// a full URL with scheme and path buries it.
function hostOf(raw: string): string {
  if (!raw) return "the identity service";
  try {
    return new URL(raw).host;
  } catch {
    return raw;
  }
}

import { Outlet } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthProvider";
import { SignInPage } from "../pages/SignInPage";
import { Loading } from "../components/StatusMessage";

// Route protection.
//
// ============================================================================
// THIS IS NOT THE AUTHORIZATION GATE. Read this before mistaking it for one.
// ============================================================================
// All this decides is what to RENDER. It does not decide what may be read or
// written -- that is settled server-side, per stream, by the identity
// verifier interceptors in component/grpc (and per row by the DSL's
// owned/granted/admin/public classification, see
// docs/public/operate/auth/per-row-authz-audit.md). An operator who deletes
// this component from a debugger gets an empty concept browser wired to a
// connection the server refuses, not access to anything.
//
// Its actual jobs are UX ones, and they matter:
//   * Don't dial an unauthenticated WebSocket the front door will reject on
//     every retry, filling an ingress log with 401s for someone who simply has
//     not signed in yet.
//   * Don't flash a sign-in page at an operator who IS signed in -- the
//     "loading" state exists because "not known yet" and "signed out" must not
//     look alike.
//   * Keep the requested URL in the address bar while signing in, so deep
//     links survive the round trip (#3316).

export function RequireAuth(): ReactNode {
  const { status } = useAuth();

  if (status === "loading") {
    return (
      <div className="flex min-h-full items-center justify-center bg-bg p-6">
        <Loading what="your session" />
      </div>
    );
  }

  // "authDisabled" renders the shell: the cluster admits every dial as the
  // synthetic local-dev cluster owner, so there is no sign-in to perform and a
  // wall here would be unpassable. The header says so out loud rather than
  // letting an operator assume they authenticated.
  if (status === "signedIn" || status === "authDisabled") {
    return <Outlet />;
  }

  return <SignInPage />;
}

import { BrowserRouter } from "react-router-dom";
import type { ReactNode } from "react";

import { AppRoutes } from "./routes";
import { AuthProvider, useAuth } from "../auth/AuthProvider";
import { ClusterProvider, type ClusterProviderProps } from "../cluster/ClusterProvider";

// The application root: router + session + cluster connection + routes.
//
// ORDER MATTERS. <AuthProvider> sits above <ClusterProvider> because the
// connection needs a credential to dial with, and the credential comes from
// the session -- not the other way round. It sits inside <BrowserRouter>
// because signing in has to remember the current route.
//
// The basename is derived from Vite's `base` rather than hardcoded. The mount
// point (/portal/) is decided in exactly one place -- vite.config.ts -- and
// the Go handler is configured to match; a second literal here is a second
// thing to keep in step. The trailing slash is stripped because react-router
// wants "/portal", not "/portal/".
//
// No props. Every seam the tests need (the identity fetch, storage, crypto,
// navigation, the dial) is a prop on the provider that owns it, and the tests
// compose those providers directly -- so this component stays the honest
// production wiring rather than a parameterised approximation of it.

export function App(): ReactNode {
  const basename = import.meta.env.BASE_URL.replace(/\/+$/, "");
  return (
    <BrowserRouter basename={basename}>
      <AuthProvider>
        <AuthenticatedCluster>
          <AppRoutes />
        </AuthenticatedCluster>
      </AuthProvider>
    </BrowserRouter>
  );
}

// AuthenticatedCluster hands the session's credential source to the
// connection, and holds the dial back until there is a session to dial with.
//
// The gate is the reason this is a component rather than two nested tags in
// App: dialling while signed out means an unauthenticated upgrade the front
// door refuses, retried behind a sign-in page the operator has not filled in
// yet -- a stream of 401s in the ingress log describing nothing that is wrong.
export function AuthenticatedCluster({
  children,
  dial,
}: {
  children: ReactNode;
  // Same seam ClusterProvider already exposes, forwarded so a test can drive
  // the whole App wiring -- session gate included -- without a server.
  dial?: ClusterProviderProps["dial"];
}): ReactNode {
  const { authSource, connectable } = useAuth();
  return (
    <ClusterProvider auth={authSource} enabled={connectable} {...(dial ? { dial } : {})}>
      {children}
    </ClusterProvider>
  );
}

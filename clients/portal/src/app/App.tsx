import { BrowserRouter } from "react-router-dom";
import type { ReactNode } from "react";

import { AppRoutes } from "./routes";
import { ClusterProvider } from "../cluster/ClusterProvider";
import type { PortalAuthSource } from "../cluster/auth";

// The application root: router + cluster connection + routes.
//
// The basename is derived from Vite's `base` rather than hardcoded. The mount
// point (/portal/) is decided in exactly one place -- vite.config.ts -- and
// the Go handler is configured to match; a second literal here is a second
// thing to keep in step. The trailing slash is stripped because react-router
// wants "/portal", not "/portal/".

export interface AppProps {
  // #3315 passes the identity-backed credential source here. See
  // src/cluster/auth.ts for why this is the only entry point for one.
  auth?: PortalAuthSource;
}

export function App({ auth }: AppProps = {}): ReactNode {
  const basename = import.meta.env.BASE_URL.replace(/\/+$/, "");
  return (
    <BrowserRouter basename={basename}>
      <ClusterProvider {...(auth ? { auth } : {})}>
        <AppRoutes />
      </ClusterProvider>
    </BrowserRouter>
  );
}

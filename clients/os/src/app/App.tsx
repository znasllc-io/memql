import { useEffect, useState } from "react";

import { AuthProvider, useAuth } from "../auth/AuthProvider";
import { SignIn } from "../chrome/SignIn";
import { Shell } from "../chrome/Shell";
import { layoutFromWindow, type ChromeLayout } from "./layout";

export function App() {
  return (
    <AuthProvider>
      <OsBoot />
    </AuthProvider>
  );
}

function OsBoot() {
  const { status, signIn, signOut, config, authSource } = useAuth();
  const [layout, setLayout] = useState<ChromeLayout>(() => layoutFromWindow(window));

  useEffect(() => {
    const update = () => setLayout(layoutFromWindow(window));
    const queries = ["(hover: hover)", "(pointer: fine)", "(pointer: coarse)"];
    const mqls = queries.map((q) => window.matchMedia(q));
    for (const mq of mqls) mq.addEventListener("change", update);
    return () => {
      for (const mq of mqls) mq.removeEventListener("change", update);
    };
  }, []);

  // NO ACCESS FETCH HERE ANY MORE (memql#4775). Boot used to read the session
  // over HTTP from `{identityUrl}/me/api/profile` -- a route nothing serves --
  // and hand the null result down as a prop, which is how every role-gated app
  // came to be invisible to everybody. The Shell now resolves it from the
  // cluster stream it already dials; see `modules/profile/useResolvedAccess`.
  //
  // Nothing replaces it at THIS level on purpose: the read needs the
  // Connection, and the Connection is created inside the Shell.

  if (status === "loading") {
    return (
      <div className="os-boot" data-os-boot="loading">
        Loading
      </div>
    );
  }
  if (status === "signed-out" || status === "unavailable") {
    return <SignIn status={status} onSignIn={signIn} />;
  }
  return <Shell layout={layout} onSignOut={signOut} config={config} authSource={authSource} />;
}

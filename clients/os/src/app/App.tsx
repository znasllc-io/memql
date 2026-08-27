import { useEffect, useState } from "react";

import { AuthProvider, useAuth } from "../auth/AuthProvider";
import { SignIn } from "../chrome/SignIn";
import { Shell } from "../chrome/Shell";
import { parseProfileAccess, type ProfileAccess } from "../modules/profile/access";
import { fetchMyAccess } from "../modules/profile/myAccess";
import { layoutFromWindow, type ChromeLayout } from "./layout";

export function App() {
  return (
    <AuthProvider>
      <OsBoot />
    </AuthProvider>
  );
}

function OsBoot() {
  const { status, signIn, signOut, config } = useAuth();
  const [layout, setLayout] = useState<ChromeLayout>(() => layoutFromWindow(window));
  const [access, setAccess] = useState<ProfileAccess | null>(null);

  useEffect(() => {
    const update = () => setLayout(layoutFromWindow(window));
    const queries = ["(hover: hover)", "(pointer: fine)", "(pointer: coarse)"];
    const mqls = queries.map((q) => window.matchMedia(q));
    for (const mq of mqls) mq.addEventListener("change", update);
    return () => {
      for (const mq of mqls) mq.removeEventListener("change", update);
    };
  }, []);

  useEffect(() => {
    if (status !== "signed-in") {
      setAccess(null);
      return;
    }
    let cancelled = false;
    (async () => {
      const facts = await fetchMyAccess(config);
      if (cancelled) return;
      setAccess(facts ? parseProfileAccess(facts) : null);
    })();
    return () => {
      cancelled = true;
    };
  }, [status, config]);

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
  return <Shell layout={layout} onSignOut={signOut} access={access} />;
}

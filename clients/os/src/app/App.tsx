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
  const { status, signIn, signOut } = useAuth();
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
  return <Shell layout={layout} onSignOut={signOut} />;
}

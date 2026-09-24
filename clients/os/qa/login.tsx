import { createRoot } from "react-dom/client";
import { useState } from "react";
import { SignInPage, signInProblem } from "../src/auth/SignInPage";
import "../src/styles/index.css";
import "../src/auth/identity.css";

// The production sign-in view over simulated data. No auth transport or
// navigator.credentials calls are available in this visual-only fixture.
const params = new URLSearchParams(location.search);
const mode = params.get("mode") || "light";
document.documentElement.setAttribute("data-theme", mode);
document.documentElement.setAttribute("data-os-theme", "graphite");
document.documentElement.style.colorScheme = mode;
function App() {
  const [fields, setFields] = useState<Record<string, string>>({});
  const [state, setState] = useState(params.get("state") || "normal");
  const external = params.get("client") === "external";
  return <SignInPage data={{ Local: params.get("hosted") !== "1", Stage: "email", AuthorizeMode: true,
    ClientID: external ? "review-app" : "os", ClientName: external ? "Review app" : "os", ClientSelfRegistered: external,
    RedirectURI: external ? "https://review.example.test/auth/callback" : `${location.origin}/auth/callback` }}
    clientId="os" fields={fields} busy={state === "pending"} passkeyPending={state === "pending"}
    problem={state === "error" ? signInProblem(new Error("webauthn: no passkey matches the asserted credential")) : undefined}
    onField={(name, text) => setFields(held => ({ ...held, [name]: text }))}
    onPasskey={() => setState("error")} onSubmit={() => setState("pending")} onLegal={() => {}} />;
}
createRoot(document.getElementById("root")!).render(<App />);

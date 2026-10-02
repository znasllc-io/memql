import { createRoot } from "react-dom/client";
import { useState } from "react";
import { IdentityFrame } from "../src/auth/IdentityFrame";
import { DeviceApproval, deviceTitle } from "../src/auth/DeviceApproval";
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

function Device() {
  const [code, setCode] = useState("");
  const [done, setDone] = useState<string>();
  const state = params.get("state");
  const data = { Done: !!done, DoneApproved: done === "approve", Pending: state === "empty" ? undefined : {
    ClientName: "MemQL for Visual Studio Code and Cursor", ClientId: "memql-vscode", UserCode: "ABCD-2345",
    SourceIP: "203.0.113.9", UserAgent: "Safari on macOS", RequestedAt: "Just now", ExpiresAt: "In 10 minutes",
  } };
  return <IdentityFrame title={deviceTitle(data)} footer={<a className="os-link" href="/login.html">Return to MemQL OS</a>}>
    {state === "error" && <div className="os-signin-problem" role="alert"><p>This page needs to be refreshed before you can continue.</p><button className="os-button" onClick={() => location.assign("?view=device")}>Refresh page</button></div>}
    <div className="os-identity-fields"><DeviceApproval data={data} code={code} onCode={setCode} busy={false} blocked={state === "error"} submit={form => setDone(form.action)} /></div>
  </IdentityFrame>;
}
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
const mobileSource = new URL(location.href); mobileSource.searchParams.delete("width");
createRoot(document.getElementById("root")!).render(params.get("width") === "mobile"
  ? <iframe title="Mobile auth preview" src={mobileSource.toString()} style={{ width: 360, height: "100dvh", border: 0 }} />
  : params.get("view") === "device" ? <Device /> : <App />);

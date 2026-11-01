import { useState } from "react";
import { Button, Caption, CopyField, Field, Input, Notice } from "../../../kit";
import { appSignInIsLive, type AppSignIn, type AppSignInsHandle } from "./useAppSignIns";

/** How long after a finished sign-in this says the machine is still checking. The check
 *  runs at once, and it is what moves the app's status. */
const CHECKING_WINDOW_MS = 2 * 60 * 1000;

function justFinished(signIn: AppSignIn | null, now: Date): boolean {
  if (signIn?.status !== "succeeded") return false;
  const ended = Date.parse(signIn.endedAt);
  return !Number.isNaN(ended) && now.getTime() - ended < CHECKING_WINDOW_MS;
}

/**
 * Sign in to an app on a machine, from wherever the person is. No browser opens on
 * the machine, so this works when they are not sitting at it: the page opens here, and
 * the app on the machine waits for them.
 *
 * Codex finishes on its own page, with a code to enter there. Claude Code's page shows
 * a code to paste back. The login itself stays on the machine.
 */
export function AppSignInAction({ app, label, signIns, now, available }: {
  app: string;
  label: string;
  signIns: AppSignInsHandle;
  now: Date;
  /** Whether Sign in can be offered: their machine, online, signed out, and a Cockpit that can run one. */
  available: boolean;
}) {
  const signIn = signIns.latest(app);
  const [code, setCode] = useState("");
  const [submittedFor, setSubmittedFor] = useState("");
  const [refusal, setRefusal] = useState("");
  const live = signIn !== null && appSignInIsLive(signIn, now);

  async function run(call: () => Promise<string>) {
    setRefusal(await call());
  }
  async function submit(id: string) {
    const answer = await signIns.submitCode(id, code);
    setRefusal(answer);
    if (answer === "") { setSubmittedFor(id); setCode(""); }
  }

  const failure = refusal || (signIn?.status === "failed" ? signIn.errorMessage : "");
  const notice = failure ? <Notice tone="error" sentence="Sign-in did not finish." detail={failure} /> : null;

  if (live) {
    const submitted = submittedFor === signIn.id;
    return <div className="fleet-app-sign-in">
      {signIn.status === "requested" || !signIn.url
        ? <Caption>Starting the sign-in on this machine.</Caption>
        : <>
          <Caption>{signIn.userCode ? "Open the sign-in page and enter this code." : "Open the sign-in page, sign in, then paste the code it shows."}</Caption>
          <a className="os-button" data-tone="primary" href={signIn.url} target="_blank" rel="noopener noreferrer">Open sign-in page</a>
          {signIn.userCode ? <CopyField value={signIn.userCode} label={`the ${label} sign-in code`} /> : null}
          {signIn.needsCode ? submitted
            ? <Caption>Finishing the sign-in on this machine.</Caption>
            : <div className="fleet-app-sign-in-code">
              <Field label="Code from the sign-in page">
                <Input id={`app-sign-in-code-${app}`} label="Code from the sign-in page" value={code} onChange={setCode} code onEnter={() => { if (code.trim() && !signIns.pending) void submit(signIn.id); }} />
              </Field>
              <Button tone="primary" disabled={!code.trim()} busy={signIns.pending} onClick={() => void submit(signIn.id)}>Finish sign-in</Button>
            </div> : null}
        </>}
      <div className="fleet-section-actions"><Button disabled={signIns.pending} onClick={() => void run(() => signIns.cancel(signIn.id))}>Cancel</Button></div>
      {notice}
    </div>;
  }
  if (justFinished(signIn, now) && available) {
    return <Caption>Signed in. Checking that {label} works on this machine.</Caption>;
  }
  if (!available) return null;
  return <>
    <div className="fleet-section-actions">
      <Button tone="primary" busy={signIns.pending} busyLabel="Starting…" ariaLabel={`Sign in to ${label}`} onClick={() => void run(() => signIns.start(app))}>Sign in</Button>
    </div>
    {notice}
  </>;
}

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { useAuth } from "../auth/AuthProvider";
import { useCluster } from "../cluster/ClusterProvider";
import { Button, ButtonLink, Container, Panel } from "../ui";
import { InferenceGate } from "./InferenceGate";
import { canSkipInference, gateStep, useInferenceStatus } from "./useInferenceStatus";
import { fleetPath } from "./urls";

// The first-run gate (epic memql#4676, task memql#4684, design D9).
//
// ===========================================================================
// IT GATES THE CONSOLE, NOT THE CLUSTER
// ===========================================================================
// Nothing here is enforced server-side and nothing here should be. The engine
// boots, serves and migrates with no provider configured (D8); features that
// need a model refuse or park typed. This component decides what a PERSON
// sees first, and deleting it from a debugger gets somebody a console whose
// AI features park -- not access to anything.
//
// ===========================================================================
// ONCE THROUGH, ALWAYS THROUGH -- FOR THIS SESSION
// ===========================================================================
// `entered` latches. A machine that goes to sleep an hour later produces a
// rail notice, never an eviction: ejecting somebody mid-session would punish
// them for a condition that fixes itself when they open the laptop, and would
// take away the page they would use to find that out.
//
// The passkey step is deliberately NOT implemented here as a second ceremony.
// The WebAuthn enrolment already exists on the identity origin (memql#3407)
// and this sends people to it; a duplicate would be a second implementation
// of a security ceremony, which is the last thing that should have two.

export function FirstRunGate({ children }: { children: ReactNode }): ReactNode {
  const { config } = useAuth();
  const { query, status: connection } = useCluster();
  const authEnabled = config.authEnabled;

  const [entered, setEntered] = useState(false);
  const [skipped, setSkipped] = useState(false);
  const [hasPasskey, setHasPasskey] = useState<boolean | null>(null);
  const inference = useInferenceStatus(connection === "connected");
  const navigate = useNavigate();

  useEffect(() => {
    // An auth-disabled cluster has no identity, so there is no passkey to
    // enrol and no query that would answer. Treating it as "has one" is what
    // makes gateStep skip the step rather than special-casing it twice.
    if (!authEnabled) {
      setHasPasskey(true);
      return;
    }
    if (!query || connection !== "connected") return;
    let stale = false;
    query
      .passkeysForSelf({})
      .then((result: unknown) => {
        if (stale) return;
        const rows = Array.isArray(result)
          ? result
          : ((result as { rows?: () => unknown } | null)?.rows?.() ?? []);
        setHasPasskey(Array.isArray(rows) && rows.length > 0);
      })
      .catch(() => {
        // A read that failed must not lock somebody out of their own console.
        // The passkey step exists to encourage enrolment, not to be a wall
        // that an unrelated outage can raise -- so an unreadable answer is
        // treated as "has one" and the person continues.
        if (!stale) setHasPasskey(true);
      });
    return () => {
      stale = true;
    };
  }, [authEnabled, query, connection]);

  const step = gateStep({
    authEnabled,
    // `null` means the passkey read has not answered. Treated as HAVING one
    // so the gate does not flash an enrolment prompt at somebody who is
    // already enrolled -- "not known yet" and "absent" must not look alike,
    // which is the same rule RequireAuth's loading state exists for.
    hasPasskey: hasPasskey !== false,
    status: inference.status,
    alreadyEntered: entered,
    skipped,
    unreadable: inference.error !== "",
  });

  useEffect(() => {
    if (step === "console") setEntered(true);
  }, [step]);

  const goPairMachine = useCallback(() => {
    setEntered(true);
    navigate(fleetPath("machines"));
  }, [navigate]);

  const goEnterKey = useCallback(() => {
    setEntered(true);
    navigate("/admin/providers");
  }, [navigate]);

  // A CLUSTER THAT IS NOT CONNECTED IS NOT GATED. The gate's question is
  // answered by the server, so with no connection there is no answer -- and
  // holding here would replace the shell's own connection error with a
  // configuration prompt, hiding the actual problem behind an unrelated one.
  // The shell says "cannot reach this cluster"; that is the message a person
  // needs.
  if (connection !== "connected") return <>{children}</>;

  if (step === "console") return <>{children}</>;

  if (step === "passkey") {
    return (
      <Container>
        <div className="py-10">
          <PasskeyStep onContinue={() => setHasPasskey(true)} />
        </div>
      </Container>
    );
  }

  return (
    <Container>
      <div className="py-10">
        <InferenceGate
          status={inference.status}
          loading={inference.loading}
          error={inference.error}
          canSkip={canSkipInference(authEnabled)}
          onPairMachine={goPairMachine}
          onEnterKey={goEnterKey}
          onRecheck={inference.reload}
          onSkip={() => setSkipped(true)}
        />
      </div>
    </Container>
  );
}

// PasskeyStep points at the enrolment ceremony that already exists rather than
// re-implementing it. "Continue" is not a skip: it is for somebody who just
// enrolled in the other tab and wants this page to stop asking.
function PasskeyStep({ onContinue }: { onContinue: () => void }): ReactNode {
  return (
    <Panel>
      <h2 className="text-lg font-medium">Add a passkey first</h2>
      <p className="mt-1 text-sm text-muted">
        A passkey is what gets you back in without a link in your inbox. It takes one touch, and it
        is the credential this cluster prefers.
      </p>
      <div className="mt-4 flex flex-wrap gap-2">
        <ButtonLink tone="primary" href="/me/devices">
          Add a passkey
        </ButtonLink>
        <Button tone="quiet" onClick={onContinue}>
          I already added one
        </Button>
      </div>
    </Panel>
  );
}

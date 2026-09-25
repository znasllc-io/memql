import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { useCallback, useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Head, Notice, Panel, Subhead, boolOr, stringsOf, useAppReach } from "../../../kit";
import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";

// Readiness: is this cluster ready to do work, and if not, where is the fix.
//
// ===========================================================================
// A SIGNPOST, NOT A GATE
// ===========================================================================
// The portal had a first-run gate over exactly this reading. It was
// client-side only and session-latched -- so it enforced NOTHING (every API
// it stood in front of was reachable the moment somebody navigated past it,
// and it never asked again once dismissed) while behaving like a wall. It was
// a signpost wearing a gate's clothes, which is the worst of both: it stopped
// nobody and it stood in front of everybody.
//
// This is the signpost with the ambush removed. It never blocks, never
// latches, never redirects, and it opens the app because "what is wrong with
// this cluster" is the first question its reader has. A cluster with
// everything in place reads as three calm lines; the notices arrive only when
// something genuinely is not ready.
//
// ===========================================================================
// TWO READINGS, SETTLING SEPARATELY
// ===========================================================================
// Deliberately not one `Promise.all`. A combined await lets the read that
// WILL be refused decide the state of the one that succeeded, and these two
// have different reasons to fail -- one is a registry projection, the other
// is a row read. Each says its own thing, in its own line.
//
// NOT KNOWING AND NOT BEING READY ARE DIFFERENT ANSWERS. A failed or refused
// read renders the server's own sentence under the line it belongs to, never
// a red "not ready" -- which would send somebody to fix a cluster whose only
// problem is that we could not ask.

export function ReadinessSection() {
  const connection = useOsConnection();
  const session = useSession();

  const readInference = useCallback(
    async (signal: AbortSignal): Promise<Row | null> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.inferenceStatus({}, { signal });
      return result.single();
    },
    [connection],
  );

  const readPasskeys = useCallback(
    async (signal: AbortSignal): Promise<Row[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.passkeysForSelf({}, { signal });
      return result.rows();
    },
    [connection],
  );

  const inference = useReading<Row | null>(
    "cluster:readiness:inference",
    connection === null ? null : readInference,
  );
  const passkeys = useReading<Row[]>(
    "cluster:readiness:passkeys",
    connection === null ? null : readPasskeys,
  );

  return (
    <div className="os-cluster">
      <Head title="Readiness" meta="nothing here blocks anything">
        <Button
          tone="quiet"
          busy={inference.state === "reading" || passkeys.state === "reading"}

          onClick={() => {
            inference.reread();
            passkeys.reread();
          }}
        >
          Read again
        </Button>
      </Head>
      <Panel label="Readiness">
        <InferenceLine reading={inference} />
        <PasskeyLine reading={passkeys} identityUrl={session.config.identityUrl} />
      </Panel>
      <Caption>
        These are the two things a cluster needs before anybody can get useful work out of it. They
        are read once when this section opens, and nothing here stops you using the rest of the
        product.
      </Caption>
    </div>
  );
}

function InferenceLine({
  reading,
}: {
  reading: ReturnType<typeof useReading<Row | null>>;
}) {
  const row = reading.value;
  const fleet = useAppReach("fleet");

  const facts = useMemo(() => {
    if (row === null) return null;
    return {
      eligible: boolOr(row, "eligible", false),
      doorsOpen: stringsOf(row, "doorsOpen"),
      localModelCount: numberOf(row, "localModelCount"),
      eligibleModelIds: stringsOf(row, "eligibleModelIds"),
      appEligible: boolOr(row, "appEligible", false),
      runnableApps: stringsOf(row, "runnableApps"),
      appSessionsInstalled: boolOr(row, "appSessionsInstalled", false),
      cloudConfigured: boolOr(row, "cloudConfigured", false),
      federationConfigured: boolOr(row, "federationConfigured", false),
      fleetCatalogInstalled: boolOr(row, "fleetCatalogInstalled", false),
      minimumContextWindow: numberOf(row, "minimumContextWindow"),
    };
  }, [row]);

  return (
    <div className="os-cluster-line">
      <Subhead>Inference</Subhead>
      {reading.state === "failed" ? (
        <Notice
          tone="info"
          sentence="We could not ask this cluster whether it can reach a model."
          next="That is not the same as a cluster with no inference -- try again, or read the bff's logs."
          detail={reading.error}
        />
      ) : null}
      {reading.state === "reading" && facts === null ? <ContentSkeleton kind="detail" label="Loading the cluster" /> : null}
      {reading.state === "read" && facts === null ? (
        <Caption>The cluster answered with no reading at all, which nothing in the engine should produce. Read the bff's logs.</Caption>
      ) : null}

      {facts === null ? null : facts.eligible ? (
        <>
          <p className="os-cluster-fact">
            This cluster can reach a model through {doorPhrase(facts.doorsOpen)}.
          </p>
          {facts.localModelCount === 0 ? null : (
            <Caption>
              {facts.eligibleModelIds.length} of {facts.localModelCount}{" "}
              {facts.localModelCount === 1 ? "model" : "models"} on your fleet meet the{" "}
              {facts.minimumContextWindow.toLocaleString()}-token floor.
            </Caption>
          )}
          {facts.appEligible ? (
            <Caption>
              {facts.runnableApps.join(", ")}{" "}
              {facts.runnableApps.length === 1 ? "is" : "are"} signed in and reachable on a machine
              you have paired.
            </Caption>
          ) : null}
        </>
      ) : (
        <Notice
          tone="warn"
          sentence="No door to a model is open, so anything that needs one will refuse."
          next={notReadyNext(facts)}
        >
          <Caption>
            {!facts.fleetCatalogInstalled
              ? "Fleet model availability is unknown."
              : facts.localModelCount === 0
                ? "Your fleet offers no models at all."
                : `Your fleet offers ${facts.localModelCount} ${
                    facts.localModelCount === 1 ? "model" : "models"
                  }, and none of them meets the ${facts.minimumContextWindow.toLocaleString()}-token floor with structured output.`}
          </Caption>
          {/* The route above says it in WORDS, which is the discipline
              useAppReach exists to force -- a Set up group rendered with no
              shell around it has nowhere to hand off to, and a button is the
              only half that can go missing. This carries the same intent the
              first-run wizard's fleet door sends, so a person arriving from
              either lands on the same panel with the same box already ticked. */}
          {facts.fleetCatalogInstalled && fleet.canOpenWindows ? (
            <Button
              onClick={() => fleet.open("machines", { addMachine: { inference: true } })}
            >
              Add a machine for local models
            </Button>
          ) : null}
        </Notice>
      )}
    </div>
  );
}

/**
 * Where to go, named by which doors are actually shut.
 *
 * Three separate fixes, and they live in three different places -- so a
 * single "configure inference" sentence would be true and useless. The fleet
 * one is named first because it is the only one that costs nothing.
 *
 * THERE IS NO KEY ROUTE ANY MORE (epic memql#5088, D5). This used to offer
 * "add a provider key", and a route that no longer exists is worse than no
 * route: somebody follows it to a section that has no field for it and
 * concludes the console is broken. Federation is the only door for a cloud
 * vendor, both vendors have one, and the two are named separately because
 * they are configured separately -- a cluster can federate with one and not
 * the other, and "configure federation" would not say which is missing.
 */
function notReadyNext(facts: {
  cloudConfigured: boolean;
  federationConfigured: boolean;
  fleetCatalogInstalled: boolean;
  appSessionsInstalled: boolean;
}): string {
  const routes: string[] = [];
  if (facts.fleetCatalogInstalled) {
    // NAMES THE CHECKBOX, not just the section (epic memql#5103, design D5).
    // "Pair a machine that runs a local model" left the last step to be
    // guessed: pairing a machine gets you a machine, and the thing that makes
    // it serve a model is a box on that panel. Naming it is what turns a
    // signpost into instructions.
    routes.push('pair a machine with "will run local models" ticked (Fleet -> Machines -> Add a machine)');
  } else {
    routes.push("the fleet inventory cannot be read here, so local model availability is unknown; try reading again");
  }
  // The app door, in the order the chain tries it: after a local model and
  // before anything metered, because it costs a subscription the person is
  // already paying for (epic memql#5096).
  if (facts.appSessionsInstalled) {
    routes.push("sign into Claude Code or Codex on a machine you have paired (Fleet -> Apps)");
  }
  if (!facts.federationConfigured) {
    routes.push(
      "set up Anthropic or OpenAI workload-identity federation (Settings -> Doors)",
    );
  }
  // THE KEY ROUTE IS GONE, and `cloudConfigured` no longer earns one of its
  // own (epic memql#5088). "Add a provider key" pointed at a box that no
  // longer exists, and `cloudConfigured` now means what federation means -- a
  // callable cloud provider is a federated one -- so naming it separately
  // would print the same sentence twice. It stays on the reading because the
  // engine still reports it; it just has nowhere of its own to send anybody.
  return `To open one: ${routes.join("; ")}.`;
}

/** The doors, in the reader's words rather than the enum's. An unrecognised
 *  value is printed as it came, never dropped: a door this build has no name
 *  for is still a door, and hiding it would under-report readiness.
 *
 *  `apiKey` loses its case with the key itself (epic memql#5088) rather than
 *  keeping a phrase for a door the engine no longer opens. It falls through to
 *  the pass-through arm above, which is the right treatment for a value from
 *  an older node: printed as it came, not silently dropped. `federation` drops
 *  the vendor name, because both vendors federate now and the reading does not
 *  say which one answered. */
function doorPhrase(doors: readonly string[]): string {
  if (doors.length === 0) return "a door it did not name";
  const named = doors.map((door) =>
    door === "local"
      ? "a local model on your fleet"
      : door === "app"
        ? "a signed-in app on one of your machines"
        : door === "federation"
          ? "workload-identity federation"
          : door,
  );
  if (named.length === 1) return named[0] as string;
  return `${named.slice(0, -1).join(", ")} and ${named[named.length - 1]}`;
}

function PasskeyLine({
  reading,
  identityUrl,
}: {
  reading: ReturnType<typeof useReading<Row[]>>;
  identityUrl: string;
}) {
  const rows = reading.value;
  // `active` defaults TRUE on the concept, and a folded row carries only what
  // a write touched -- so it is read through boolOr with that default rather
  // than through a bare truthiness test that would read an absent field as a
  // revoked passkey.
  const live = (rows ?? []).filter((row) => boolOr(row, "active", true));
  const devicesUrl = identityUrl === "" ? "" : `${identityUrl.replace(/\/$/, "")}/me/devices`;

  return (
    <div className="os-cluster-line">
      <Subhead>Your passkey</Subhead>
      {reading.state === "failed" ? (
        <Notice
          tone="info"
          sentence="We could not read your passkeys."
          next="That says nothing about whether you have one."
          detail={reading.error}
        />
      ) : null}
      {reading.state === "reading" && rows === null ? <ContentSkeleton kind="detail" label="Loading the cluster" /> : null}

      {rows === null ? null : live.length > 0 ? (
        <>
          <p className="os-cluster-fact">
            You have {live.length} {live.length === 1 ? "passkey" : "passkeys"} on this account.
          </p>
          <Caption>
            {devicesUrl === ""
              ? "Manage them on the identity service, under Devices."
              : null}
            {devicesUrl === "" ? null : (
              <a href={devicesUrl} target="_blank" rel="noreferrer noopener">
                Manage them under Devices
              </a>
            )}
          </Caption>
        </>
      ) : (
        <Notice
          tone="warn"
          sentence="You have no passkey on this account."
          next="A sign-in link is the only way back into it, so losing access to your mailbox loses the account."
        >
          {devicesUrl === "" ? (
            <Caption>Register one on the identity service, under Devices.</Caption>
          ) : (
            <Caption>
              <a href={devicesUrl} target="_blank" rel="noreferrer noopener">
                Register one under Devices
              </a>
            </Caption>
          )}
        </Notice>
      )}
    </div>
  );
}

/** A numeric field, defaulting to zero. Distinct from `Figure`: these are
 *  counts the engine always reports, and an absent one here is a malformed
 *  reading rather than a state with its own meaning. */
function numberOf(row: Row, key: string): number {
  const raw = row[key];
  if (typeof raw === "number" && Number.isFinite(raw)) return raw;
  if (typeof raw === "string" && raw.trim() !== "") {
    const parsed = Number(raw);
    if (Number.isFinite(parsed)) return parsed;
  }
  return 0;
}

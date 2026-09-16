import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { createWorkerToken, revokeWorkerToken } from "@znasllc-io/memql-sdk-core/identity";

import { useSession } from "../../../chrome/access";
import { localCockpitInstall } from "./localInstall";
import { useModelResponse } from "./useModelResponse";
import { isWorkerOnline } from "../online";
import { useOsConnection } from "../../../live/connection";
import { useMachines } from "../../../live/machines";
import { useNow } from "../../../kit/useNow";
import { machineFromRow, type MachineRow } from "../rows";
import { useMachineWrites } from "../machines/useMachineWrites";
import { useModelPulls } from "../machines/useModelPulls";
import { machineModelsFrom } from "../machines/models";
import {
  EMPTY_DRAFT,
  barFor,
  checksFor,
  matchRegistration,
  phaseOf,
  stopsFor,
  type Check,
  type Draft,
  type FlowBar,
  type FlowFacts,
  type FlowStop,
  type Mint,
  type Phase,
} from "./flow";

// THE FLOW'S STATE, held by the Fleet app rather than by the page (design
// record 2026-09-08-cockpit-install-wizard, D7).
//
// ===========================================================================
// IT SURVIVES THE WINDOW'S OWN NAVIGATION, AND NOTHING ELSE
// ===========================================================================
// A person who clicks Routing while a download runs on their machine and
// comes back to Machines finds the wizard where they left it -- the token
// still on screen, the cluster still listening. That is why this hook is
// called by FleetApp, which stays mounted across section changes, and not by
// the page, which does not.
//
// It lives in React state ONLY. The token is never written to localStorage,
// sessionStorage or a URL: this credential does not expire in fifteen
// minutes, it lets a machine act as its owner's worker, and a copy in browser
// storage outlives the tab. The OS persists a great deal to localStorage --
// desks, surfaces, pins, the Fleet settings -- which is exactly why the
// exception has to be stated here and asserted by the tests. Closing the
// window is therefore the same act as "leave, keep the token", and the
// Install stop says so in one sentence.
//
// ===========================================================================
// THE REGISTRATION IS MATCHED, NEVER COUNTED (D3)
// ===========================================================================
// The mint reply carries the identity the machine will authenticate as, and
// the registration row carries the same id. Watching the population GROW --
// the previous panel's method -- reported the wrong machine when two people
// added one at once, and hid a real arrival behind a revocation elsewhere.

export interface AddMachineFlow {
  /** Whether the page is shown in place of the list. */
  active: boolean;
  draft: Draft;
  phase: Phase;
  facts: FlowFacts;
  checks: Check[];
  stops: FlowStop[];
  bar: FlowBar;
  /** The rename's refusal, verbatim, or "". A refused rename changes nothing
   *  else: the machine keeps its hostname. */
  renameError: string;
  /** Ask the matched machine to pull the catalog's recommended set (D13).
   *  Legal only once it has registered and reports a runtime; the Checks
   *  stop offers it exactly then. */
  pullRecommended: () => Promise<void>;
  pulling: boolean;
  /** The cluster's refusal of the pull, verbatim, or "". */
  pullError: string;
  retryResponse: () => void;
  /** Open the page. `inference` pre-selects the local-models choice, which
   *  is the ONLY seam through which the flag arrives pre-set (epic
   *  memql#5106, D4): a pre-ticked download of several gigabytes needs an
   *  act behind it that said so. A flow already past its mint is left alone. */
  start: (preset: { inference?: boolean }) => void;
  setDraft: (patch: Partial<Draft>) => void;
  mint: () => Promise<void>;
  /** Cancel: before a mint, leaves; after one, asks which of two things. */
  cancel: () => void;
  keepWaiting: () => void;
  leaveKeepToken: () => void;
  revokeAndLeave: () => Promise<void>;
  /** Leave with the machine registered. Returns the registration id to open,
   *  or "" when there is none. */
  finish: () => string;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The platform this browser reports, as a default for the choice -- a
 *  person adding the computer they are sitting at picks nothing. Anything
 *  that is not recognisably Linux defaults to macOS, the previous default. */
function platformDefault(): Draft["platform"] {
  const platform = globalThis.navigator?.platform ?? "";
  return /linux/i.test(platform) ? "linux" : "mac";
}

export function useAddMachineFlow(): AddMachineFlow {
  const connection = useOsConnection();
  const { config } = useSession();
  const localTest = localCockpitInstall(config.domain) !== null;
  const { collection } = useMachines();
  const writes = useMachineWrites();
  const now = useNow(15_000);

  const [active, setActive] = useState(false);
  const [draft, setDraftState] = useState<Draft>(EMPTY_DRAFT);
  const [minting, setMinting] = useState(false);
  const [mintError, setMintError] = useState("");
  const [mint, setMint] = useState<Mint | null>(null);
  const [mintedAt, setMintedAt] = useState<Date | null>(null);
  const [cancelAsked, setCancelAsked] = useState(false);
  const [revoking, setRevoking] = useState(false);
  const [revokeError, setRevokeError] = useState("");
  const [beats, setBeats] = useState(0);

  // The registration feed, read as it changes. The provider re-renders its
  // consumers on every fold, but reading the snapshot through the store
  // contract is what makes this correct rather than incidental.
  const subscribe = useCallback(
    (listener: () => void) => (collection ? collection.subscribe(listener) : () => {}),
    [collection],
  );
  const snapshot = useSyncExternalStore(subscribe, () => collection?.snapshot ?? null, () => null);
  const rows = useMemo(() => (snapshot ? snapshot.rows.map(machineFromRow) : []), [snapshot]);

  const machine: MachineRow | null = useMemo(
    () => (mint === null ? null : matchRegistration(rows, mint.identityId)),
    [rows, mint],
  );

  // HEARTBEATS SINCE THE REGISTRATION. The registration's own lastSeenAt is
  // beat zero; every DISTINCT value after it is one heartbeat heard. Distinct
  // rather than "changed", because a fold can deliver the same row twice.
  const beatsRef = useRef<{ id: string; seen: Set<string> }>({ id: "", seen: new Set() });
  useEffect(() => {
    if (machine === null) return;
    const held = beatsRef.current;
    if (held.id !== machine.id) {
      beatsRef.current = { id: machine.id, seen: new Set([machine.lastSeenAt]) };
      setBeats(0);
      return;
    }
    if (held.seen.has(machine.lastSeenAt)) return;
    held.seen.add(machine.lastSeenAt);
    setBeats(held.seen.size - 1);
  }, [machine]);

  // THE ONE RENAME (D8): once, when the matched registration first appears,
  // the name typed at the start becomes the machine's display name. Keyed on
  // the registration id, so a heartbeat -- which changes the row object and
  // nothing a person said -- never fires it again.
  const renamedRef = useRef("");
  const [renameError, setRenameError] = useState("");
  useEffect(() => {
    if (machine === null) return;
    if (renamedRef.current === machine.id) return;
    renamedRef.current = machine.id;
    const name = draft.name.trim();
    if (name === "" || machine.displayName.trim() === name) return;
    void writes.rename(machine.id, name).then((ok) => {
      if (!ok) setRenameError("The name was not applied; the machine keeps its hostname.");
    });
  }, [machine, draft.name, writes]);

  // THE PULLS ONTO THE MATCHED MACHINE, live. The feed keys on the machine
  // and is empty until one is matched; the models check draws a running pull
  // as the machine moving, and settles from the labels when the model is
  // re-advertised rather than from the pull's own end.
  const pulls = useModelPulls(machine?.id ?? "");
  const failedPull = useMemo(() => {
    // The feed is newest first. A later download of another model cannot
    // clear a failure; only a newer attempt of the same model supersedes it.
    const seen = new Set<string>();
    const advertised = new Set(machineModelsFrom(machine?.reportedLabels ?? {}).map((model) => model.modelId));
    return pulls.pulls.find((pull) => {
      if (seen.has(pull.model)) return false;
      seen.add(pull.model);
      return !advertised.has(pull.model) && (pull.status === "failed" || pull.status === "cancelled");
    }) ?? null;
  }, [pulls.pulls, machine?.reportedLabels]);
  const [pulling, setPulling] = useState(false);
  const [pullError, setPullError] = useState("");
  const pullRecommended = useCallback(async () => {
    if (connection === null || machine === null || pulling) return;
    setPulling(true);
    setPullError("");
    try {
      await connection.query.fleetPullRecommended({ registrationId: machine.id });
    } catch (err: unknown) {
      setPullError(describe(err));
    } finally {
      setPulling(false);
    }
  }, [connection, machine, pulling]);

  const reset = useCallback(() => {
    setActive(false);
    setPulling(false);
    setPullError("");
    setDraftState(EMPTY_DRAFT);
    setMinting(false);
    setMintError("");
    setMint(null);
    setMintedAt(null);
    setCancelAsked(false);
    setRevoking(false);
    setRevokeError("");
    setBeats(0);
    setRenameError("");
    beatsRef.current = { id: "", seen: new Set() };
    renamedRef.current = "";
  }, []);

  const start = useCallback(
    (preset: { inference?: boolean }) => {
      setActive(true);
      // A flow past its mint holds a credential; an intent arriving now must
      // not throw it away.
      setMint((held) => {
        if (held === null) {
          setDraftState({
            ...EMPTY_DRAFT,
            platform: platformDefault(),
            computerUse: localTest,
            userLocal: localTest,
            inference: preset.inference === true,
          });
        }
        return held;
      });
    },
    [localTest],
  );

  const setDraft = useCallback((patch: Partial<Draft>) => {
    setDraftState((held) => { const next = { ...held, ...patch }; return localTest && next.platform === "mac" ? { ...next, computerUse: true, userLocal: true } : next; });
  }, [localTest]);

  const mintToken = useCallback(async () => {
    const name = draft.name.trim();
    if (connection === null || name === "" || minting || mint !== null) return;
    setMinting(true);
    setMintError("");
    try {
      const result = await createWorkerToken(connection.dispatcher, { name });
      if (!result.success || result.plainToken === "") {
        setMintError(
          result.errorMessage || result.errorCode || "The cluster refused the mint and said nothing about why.",
        );
        return;
      }
      setMint({ token: result.plainToken, identityId: result.identityId });
      setMintedAt(new Date());
    } catch (err: unknown) {
      setMintError(describe(err));
    } finally {
      setMinting(false);
    }
  }, [connection, draft.name, minting, mint]);

  const cancel = useCallback(() => {
    if (mint === null) {
      reset();
      return;
    }
    setCancelAsked(true);
  }, [mint, reset]);

  const keepWaiting = useCallback(() => {
    setCancelAsked(false);
    setRevokeError("");
  }, []);

  const leaveKeepToken = useCallback(() => reset(), [reset]);

  const revokeAndLeave = useCallback(async () => {
    if (mint === null || connection === null || revoking) return;
    setRevoking(true);
    setRevokeError("");
    try {
      const result = await revokeWorkerToken(connection.dispatcher, mint.identityId);
      if (!result.success) {
        setRevokeError(result.errorMessage || result.errorCode || "the cluster said nothing about why");
        return;
      }
      reset();
    } catch (err: unknown) {
      setRevokeError(describe(err));
    } finally {
      setRevoking(false);
    }
  }, [connection, mint, revoking, reset]);

  const finish = useCallback(() => {
    const id = machine?.id ?? "";
    reset();
    return id;
  }, [machine, reset]);

  const facts: FlowFacts = useMemo(
    () => ({
      draft,
      connected: connection !== null,
      minting,
      mintError,
      mint,
      mintedAt,
      machine,
      beats,
      cancelAsked,
      revokeError,
      revoking,
      now,
    }),
    [draft, connection, minting, mintError, mint, mintedAt, machine, beats, cancelAsked, revokeError, revoking, now],
  );

  const preliminaryChecks = useMemo(
    () => (machine === null ? [] : checksFor(draft, machine, beats, now, {
      live: pulls.live,
      failed: failedPull,
      feedError: pulls.feedError,
      loading: pulls.loading,
    })),
    [draft, machine, beats, now, pulls.live, failedPull, pulls.feedError, pulls.loading],
  );
  const chatModel = machineModelsFrom(machine?.reportedLabels ?? {}).find(model => !model.embeddings)?.modelId ?? "";
  const { response, retry: retryResponse } = useModelResponse(
    active && draft.inference ? machine?.id ?? "" : "",
    chatModel,
    active && draft.inference && machine !== null && isWorkerOnline(machine, now)
      && preliminaryChecks.find(check => check.id === "models")?.state === "done",
  );
  const checks = useMemo(() => machine === null ? [] : checksFor(draft, machine, beats, now, {
    live: pulls.live, failed: failedPull, feedError: pulls.feedError, loading: pulls.loading,
  }, response), [draft, machine, beats, now, pulls.live, failedPull, pulls.feedError, pulls.loading, response]);
  const stops = useMemo(() => stopsFor(facts, checks), [facts, checks]);
  const bar = useMemo(() => barFor(facts, checks), [facts, checks]);

  return {
    active,
    draft,
    phase: phaseOf(facts),
    facts,
    checks,
    stops,
    bar,
    renameError,
    pullRecommended,
    pulling,
    pullError,
    retryResponse,
    start,
    setDraft,
    mint: mintToken,
    cancel,
    keepWaiting,
    leaveKeepToken,
    revokeAndLeave,
    finish,
  };
}

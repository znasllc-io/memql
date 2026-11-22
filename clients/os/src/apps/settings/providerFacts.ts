import { useCallback, useEffect, useMemo, useState } from "react";

import { useOsConnection } from "../../live/connection";

// Data wiring for Settings -> AI providers (epic memql#4984; the surface it
// replaces was the portal's, epic memql#4440).
//
// REQUEST/REPLY WITH AN EXPLICIT REFRESH, not a subscription, and the reason
// is sharper here than on the sections beside it: `providerAuthStatus` is a
// projection of THIS NODE's in-memory provider registry, not of rows anyone
// writes, so there is no graph event to subscribe to. A panel that appeared
// live while showing a registry that stopped moving would invite an operator
// to trust a reading taken minutes ago -- which on this surface is the
// difference between "the key took" and "the key took on the replica you
// happened to reach".
//
// WHICH REPLICA ANSWERED IS NOT KNOWABLE FROM HERE, and the section says so
// rather than implying a fleet-wide reading. The front door routes each call
// independently, so two Refreshes can be answered by two nodes. That is why
// Apply broadcasts rather than relying on repeated reads.
//
// THERE IS NO KEY HERE ANY MORE (epic memql#5088, D5). `saveKey` and the
// key-sealing builtin it called are deleted, not disabled: federation is the
// only door for a cloud vendor, and the fleet is the other door. What is left
// on the write side is the federation id form, per vendor, and Apply.

export interface ProviderStatusRow {
  name: string;
  vendor: string;
  model: string;
  available: boolean;
  authSource: string;
  reason: string;
}

type RowBag = Record<string, unknown>;

function str(row: RowBag, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function bool(row: RowBag, key: string): boolean {
  const v = row[key];
  if (typeof v === "boolean") return v;
  return typeof v === "string" && v.toLowerCase() === "true";
}

/**
 * Absorb both shapes a caller can hand back: the SDK's Result, and a plain
 * array (what a section test constructs). A test should not have to build an
 * SDK Result to say what the server returned.
 */
function materialize(result: unknown): RowBag[] {
  if (Array.isArray(result)) return result as RowBag[];
  const bag = result as { rows?: () => unknown } | null;
  if (bag && typeof bag.rows === "function") {
    const rows = bag.rows();
    if (Array.isArray(rows)) return rows as RowBag[];
  }
  return [];
}

export function toProviderRows(rows: readonly RowBag[]): ProviderStatusRow[] {
  return rows.map((row) => ({
    name: str(row, "name"),
    vendor: str(row, "vendor"),
    model: str(row, "model"),
    available: bool(row, "available"),
    authSource: str(row, "authSource"),
    reason: str(row, "reason"),
  }));
}

export type ProviderTone = "unconfigured" | "partial" | "ready";

/**
 * What the section's opening line says, as a pure function so the three states
 * can be tested without rendering.
 *
 * THE KEYLESS STATE IS NOT AN ERROR STATE. "No AI provider is configured" is
 * how a correctly-installed cluster starts -- installing spends no inference
 * and asks for no key -- so the copy has to read as a next step rather than a
 * fault. An operator who meets a red banner on a fresh install concludes the
 * install failed.
 */
export function summarize(rows: readonly ProviderStatusRow[]): {
  tone: ProviderTone;
  headline: string;
} {
  const total = rows.length;
  const available = rows.filter((r) => r.available).length;
  if (total === 0 || available === 0) {
    return {
      tone: "unconfigured",
      headline: "No AI provider is configured yet, which is how a cluster is installed.",
    };
  }
  if (available < total) {
    return { tone: "partial", headline: `${available} of ${total} providers can be called.` };
  }
  return { tone: "ready", headline: `All ${total} providers can be called.` };
}

/** Vendor id to the name a person would use for it. */
export const VENDOR_LABELS: Record<string, string> = {
  anthropic: "Anthropic",
  openai: "OpenAI",
};

export function vendorLabel(vendor: string): string {
  return VENDOR_LABELS[vendor.toLowerCase()] ?? vendor;
}

/**
 * Where the tier names become sentences.
 *
 * The distinction that matters most is env versus the two row tiers: a value
 * the pod's environment supplies is the one source a save from here cannot
 * change, so an operator who saves an id and sees no change has to be TOLD
 * that rather than left to work it out.
 *
 * Every entry is kept even though the AI vendors can no longer resolve a
 * sealed key (epic memql#5088): this maps whatever tier name the engine sends
 * and an unknown one falls through to the raw string, which is the honest
 * floor. Deleting a mapping would render a tier as a bare enum value.
 */
const AUTH_SOURCE_COPY: Record<string, string> = {
  federation: "workload identity -- nothing is stored anywhere",
  globalSecret: "a sealed row in this cluster",
  globalVariable: "a plaintext row in this cluster",
  env: "this pod's environment -- a row saved here will not override it",
  unresolved: "nothing configured",
};

export function sourceCopy(source: string): string {
  return AUTH_SOURCE_COPY[source] ?? source;
}

// ---------------------------------------------------------------------------
// The doors (epic memql#5088)
// ---------------------------------------------------------------------------

/**
 * The vendors this cluster can federate with, in the order the section shows
 * them, and the ids each one's rule is made of.
 *
 * THE PROJECTED TOKEN PATH IS NOT HERE, and its absence is the decision. It is
 * a deployment fact -- every engine Deployment already carries
 * `MEMQL_AI_ANTHROPIC_IDENTITY_TOKEN_FILE` and `MEMQL_AI_OPENAI_IDENTITY_TOKEN_FILE`
 * beside the projected volume they name (deploy/k8s/base) -- so a text box for
 * it in a browser could only ever disagree with the mount, and a path that
 * disagrees with the mount is a vendor no node can sign in to, for a reason the
 * person who typed it cannot see. The panel says where it went, because an
 * operator who used the old five-field form will look for it.
 *
 * `serviceAccountId` appears under BOTH vendors on purpose: the id means the
 * same thing to each, and the `vendor` argument is what decides which env name
 * the engine writes it to. Two differently-spelled fields for one concept
 * would be a name the operator has to translate.
 */
export interface FederationField {
  key: string;
  label: string;
  /** Shown in the empty box. A shape, never an example that could be pasted. */
  hint: string;
  required: boolean;
}

// A HINT IS ONLY EVER A PREFIX THIS REPO CAN POINT AT. `svac_` and `fdrl_` are
// Anthropic's, documented on its own ConfigMap
// (deploy/k8s/base/anthropic-federation.yaml). OpenAI's two ids have NO hint
// because nothing here knows their shape -- its ConfigMap deliberately names
// none -- and a guessed prefix in an empty box is worse than an empty box: an
// operator who pastes a correct id that does not match it will go looking for
// the mistake in their own console.
//
// The two service-account hints were swapped when this section was written:
// OpenAI's box showed `svac_...`, which is Anthropic's, while Anthropic's --
// the one that really is `svac_` -- showed nothing. Invisible in jsdom, which
// renders no placeholder, and found by looking at the screen.
export const FEDERATION_FIELDS: Record<string, readonly FederationField[]> = {
  anthropic: [
    { key: "ruleId", label: "Federation rule id", hint: "fdrl_...", required: true },
    { key: "organizationId", label: "Organization id", hint: "UUID", required: true },
    { key: "serviceAccountId", label: "Service account id", hint: "svac_...", required: true },
    {
      key: "workspaceId",
      label: "Workspace id",
      hint: "Only for a rule spanning more than one workspace",
      required: false,
    },
  ],
  openai: [
    { key: "identityProviderId", label: "Identity provider id", hint: "", required: true },
    { key: "serviceAccountId", label: "Service account id", hint: "", required: true },
  ],
};

export function federationFields(vendor: string): readonly FederationField[] {
  return FEDERATION_FIELDS[vendor.toLowerCase()] ?? [];
}

/** Which required ids a draft is still missing, in declaration order. */
export function missingFederationFields(
  vendor: string,
  draft: Readonly<Record<string, string>>,
): readonly FederationField[] {
  return federationFields(vendor).filter(
    (f) => f.required && (draft[f.key] ?? "").trim() === "",
  );
}

/**
 * What this vendor's door is, read from the registry THIS node answered with.
 *
 *  - `open`  -- ids applied, the exchange is the credential, nothing is stored.
 *  - `unset` -- no ids. NORMAL: it is how every cluster is installed. Not a
 *               fault, not amber.
 *  - `half`  -- some ids set and some missing. Every node refuses the VENDOR
 *               on this (it keeps running, and cannot use the vendor), so it
 *               is the one state that is actionable and urgent. The token
 *               file's path on its own is not this state: every Deployment
 *               sets it, and the engine reads "no id" as not set up.
 *
 * `half` IS CHECKED FIRST, and the order is the fail-safe direction: a stale
 * federated row alongside a half-configured one must not let the dangerous
 * state hide behind the reassuring one. (The two cannot both be true of one
 * auth block, so in practice the order never decides anything -- which is
 * exactly when to pick the direction that is safe if it ever does.)
 *
 * Detection is the engine's own sentence, not a field: `anthropicCredential`
 * writes "is HALF-CONFIGURED for ... federation: X set, Y missing" and hands it
 * to the provider entry as its unavailable reason. Matching the words rather
 * than parsing them is deliberate -- the sentence already names both halves
 * better than a re-derivation here would, and it is rendered verbatim.
 */
export type DoorState = "open" | "unset" | "half";

export interface VendorDoor {
  state: DoorState;
  /** The engine's own words for a half-set door. Empty otherwise. */
  said: string;
}

const HALF_CONFIGURED = "half-configured";

export function doorFor(vendor: string, rows: readonly ProviderStatusRow[]): VendorDoor {
  // Prefix, not equality: one vendor registers many provider TYPES
  // (`openai`, `openaistt`, `openaiwhisper`, ...) off one auth block, and the
  // engine's own `providerAuthEnvKeyFor` matches them the same way.
  const want = vendor.toLowerCase();
  const mine = rows.filter((r) => r.vendor.toLowerCase().startsWith(want));

  const halfway = mine.find((r) => r.reason.toLowerCase().includes(HALF_CONFIGURED));
  if (halfway) return { state: "half", said: halfway.reason };
  if (mine.some((r) => r.authSource === "federation")) return { state: "open", said: "" };
  return { state: "unset", said: "" };
}

/**
 * Whether a vendor can FETCH this cluster's public keys from its OIDC issuer.
 *
 * It decides wording, never whether the form is offered. A local cluster's
 * issuer is private, so a vendor cannot fetch its keys; both vendors accept
 * them pasted in instead, and then the ids work as anywhere. Settings > Vendors
 * says so beside the form on a local cluster. This comment used to say a local
 * cluster could never federate (design D6 of the federation epic), which the
 * vendors' own documentation contradicts (design record
 * 2026-11-21-vendor-accounts-and-api-keys, A6).
 *
 * THE OS HAD NO LOCALITY CONVENTION, so this MIRRORS the engine's rather than
 * inventing a second one: `integrations/email/delivery.go` `IsLocalDomain`,
 * which decides the same question (may this install behave as a private one)
 * from the same input. Its three shapes are reproduced exactly -- a loopback
 * literal, anything under the RFC 6761 `.localhost` TLD, and the
 * `*.local.<domain>` dev wildcard whose SECOND label is `local`.
 *
 * ONE DELIBERATE DIFFERENCE, AND IT IS THE THIRD ANSWER. The Go predicate
 * reads an empty domain as local, because a process with no domain configured
 * is not a cloud install. In a browser an empty `config.domain` means the
 * runtime config has not landed yet, which is not the same claim -- and
 * flashing a local cluster's instruction at a cloud operator for one frame is
 * a sentence the surface would have no way to take back.
 * So the answer is `unknown`, and the surface holds the sentence rather than
 * guessing which way to be wrong.
 *
 * A Go-side parity test (the shape of `component/worker/online_client_parity_test.go`)
 * belongs on the engine half of this epic; without one the two predicates are
 * held together by this comment alone.
 */
export type IssuerReach = "local" | "reachable" | "unknown";

export function localityOf(domain: string): IssuerReach {
  const d = domain.trim().toLowerCase();
  if (d === "") return "unknown";
  if (d === "localhost" || d === "127.0.0.1" || d === "::1" || d === "0.0.0.0") return "local";
  if (d.endsWith(".localhost")) return "local";
  const labels = d.split(".");
  return labels.length >= 3 && labels[1] === "local" ? "local" : "reachable";
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

export interface ProvidersState {
  rows: ProviderStatusRow[];
  loading: boolean;
  error: string;
  /** When the answer on screen was taken, or null before the first one. */
  fetchedAt: number | null;
  reload: () => void;
}

export function useProviderRegistry(enabled: boolean): ProvidersState {
  const connection = useOsConnection();
  const [rows, setRows] = useState<ProviderStatusRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  // An epoch counter, not a cache invalidation protocol: "what does a node say
  // right now" has no cache to invalidate.
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (!enabled || connection === null) return;
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void connection.query
      .providerAuthStatus({}, { signal: controller.signal })
      .then((result) => {
        if (stale) return;
        setRows(toProviderRows(materialize(result)));
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (stale) return;
        // A server-side refusal arrives as a rejected promise carrying the
        // engine's own words. Rendered in-surface, never rewritten -- an admin
        // reading "providerAuthStatus is owner-only" has been told exactly what
        // happened, which no paraphrase of ours would improve on.
        setRows([]);
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled, epoch]);

  return { rows, loading, error, fetchedAt, reload };
}

// THE FLEET DOOR MOVED, AND IT IS NOT HERE ANY MORE (epic memql#5153).
// `fleetDoorFrom` / `useFleetDoor` read `inferenceStatus` and turned it into a
// door state and a sentence; `routingFacts.doorReadings` now answers that for
// all FOUR doors at once, out of one read, so keeping a second reading of the
// same rows here would be a second place for them to disagree -- and the
// disagreement would be invisible to both. Its four sentences are carried over
// verbatim, and its tests moved with them.

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

export interface ProviderActionState {
  busy: boolean;
  message: string;
  failed: boolean;
}

export const IDLE_PROVIDER_ACTION: ProviderActionState = {
  busy: false,
  message: "",
  failed: false,
};

export interface ProviderActions {
  state: ProviderActionState;
  saveFederation: (vendor: string, fields: Record<string, string>) => Promise<void>;
  verify: (provider: string) => Promise<void>;
  apply: () => Promise<void>;
}

/** Read the single-row reply these actions return. An empty reply is reported
 *  as such rather than defaulted: an action whose result cannot be read has
 *  not been shown to have worked. */
function firstRow(result: unknown): RowBag | null {
  const rows = materialize(result);
  return rows.length > 0 ? (rows[0] ?? null) : null;
}

export function useProviderActions(onChanged: () => void): ProviderActions {
  const connection = useOsConnection();
  const [state, setState] = useState<ProviderActionState>(IDLE_PROVIDER_ACTION);

  const run = useCallback(
    async (work: () => Promise<string>): Promise<void> => {
      setState({ busy: true, message: "", failed: false });
      try {
        const message = await work();
        setState({ busy: false, message, failed: false });
        onChanged();
      } catch (err: unknown) {
        setState({
          busy: false,
          message: err instanceof Error ? err.message : String(err),
          failed: true,
        });
      }
    },
    [onChanged],
  );

  // THE VENDOR IS AN ARGUMENT, NOT A ROW NAME (epic memql#5088). Both vendors
  // federate now and one of the ids is spelled the same for each, so the write
  // has to say which set of env names it is filling. Sending the vendor rather
  // than letting the engine guess from which ids arrived is what keeps a
  // half-typed Anthropic rule from being read as a complete OpenAI one.
  //
  // THE CAST IS A SEAM, AND IT IS LOAD-BEARING UNTIL THE DSL CATCHES UP.
  // `ProviderFederationSetArgs` is GENERATED from `dsl/common/builtins.memql`,
  // which at the time of writing still declares the five Anthropic ids and
  // neither `vendor` nor `identityProviderId`.
  //
  // Worse than the type error is what the generated client does with an
  // undeclared key. `QueryClient.prototype.providerFederationSet` renders its
  // call through `buildProviderFederationSet`, which pushes only the fields it
  // knows about -- so an undeclared `vendor` is DROPPED ON THE FLOOR rather
  // than refused, and the engine sees a write with no vendor on it. THREE
  // things have to land for this call to reach the engine as written:
  //
  //   1. `vendor string!` on the `providerFederationSet` builtin,
  //   2. `identityProviderId string` beside it (OpenAI's half; the two vendors
  //      share `serviceAccountId` deliberately, so only this one is new),
  //   3. `make sdk-gen`.
  //
  // Until all three land, a mocked test here proves this client's intent and
  // NOTHING about the wire -- the mock replaces the very builder that does the
  // dropping. Delete the cast when the regenerated type carries the fields;
  // tsc will say so on the next typecheck.
  const saveFederation = useCallback(
    async (vendor: string, fields: Record<string, string>) =>
      run(async () => {
        if (connection === null) throw new Error("Not connected to the cluster.");
        const args = { vendor, ...fields } as Parameters<
          typeof connection.query.providerFederationSet
        >[0];
        const row = firstRow(await connection.query.providerFederationSet(args));
        if (row === null) throw new Error("the cluster returned no result for the federation write");
        return str(row, "message");
      }),
    [connection, run],
  );

  const verify = useCallback(
    async (provider: string) =>
      run(async () => {
        if (connection === null) throw new Error("Not connected to the cluster.");
        const row = firstRow(await connection.query.providerVerify({ provider }));
        if (row === null) throw new Error("the cluster returned no verification result");
        // A REFUSAL IS A RESULT, not a thrown error: the engine returns
        // verified=false with the vendor's own words, and rendering that as an
        // exception would blame this console for the vendor's answer.
        if (bool(row, "verified")) return `${provider}: the vendor accepted this credential.`;
        throw new Error(
          `${provider}: ${str(row, "reason") || "the vendor did not accept this credential."}`,
        );
      }),
    [connection, run],
  );

  const apply = useCallback(
    async () =>
      run(async () => {
        if (connection === null) throw new Error("Not connected to the cluster.");
        const row = firstRow(await connection.query.providersReload({}));
        if (row === null) throw new Error("the cluster returned no result for the reload");
        return (
          `Reloaded. The node that answered can call ${String(row["availableOnThisNode"] ?? "?")} ` +
          `of ${String(row["registered"] ?? "?")} providers, and every other node was told to re-resolve.`
        );
      }),
    [connection, run],
  );

  return useMemo(
    () => ({ state, saveFederation, verify, apply }),
    [state, saveFederation, verify, apply],
  );
}

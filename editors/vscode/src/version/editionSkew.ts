// The cluster's MemQL language against this extension's, at connect
// (memql#5362; D25 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// WHY IT EXISTS. This extension ships its own language server, so completion
// and diagnostics are the grammar the extension was PACKAGED with. A cluster
// speaks the grammar IT was built with. The extension used to learn only the
// cluster's engine release at connect, so nothing compared the two languages:
// an editor older than its cluster went on offering forms the cluster had
// retired, one newer offered forms the cluster refuses, and neither said so.
// The cluster now states its language on the handshake, and package.json
// states this extension's (the `memql` block, held to the parser by
// cmd/memql-lsp/editorparity_test.go).
//
// FIVE STATES, and two of them say nothing:
//
//   unknown       the cluster reported no edition -- it predates the handshake
//                 fields -- or one side has nothing to compare. Nothing can be
//                 shown, so nothing is.
//   match         the same edition and the same grammar.
//   clusterNewer  the cluster's edition is later, or its grammar first shipped
//                 in a release newer than this one. A warning naming the release
//                 to install.
//   clusterOlder  the cluster's edition is earlier, or its grammar first shipped
//                 in a release older than this one. Information: the editor may
//                 suggest forms the cluster refuses, and nothing installed here
//                 changes a cluster.
//   differs       they differ and no order can be shown. Information naming
//                 both.
//
// WHAT MAY BE ORDERED, AND WHAT MAY NOT. A grammar version is a LABEL --
// `<year>.<month>-<slug>-<8 hex>` -- and two labels are only equal or
// different. This module orders exactly two things: an edition against an
// edition (editions are years), and the release that first carried the
// cluster's grammar (ServerHello.editor_release) against this extension's own
// version, both releases compared by compare.ts. Anything neither can decide
// is `differs`, and its words say the order cannot be shown. That is
// describe.ts's discipline one layer over: "cannot tell" is never "current",
// and it is never "newer" either.
//
// THE NOTICE READS AS WHAT IT IS. Plain, active sentences that name both sides
// or the release, never an apology -- and never a claim the facts cannot
// carry, the rule skewHint.ts states for its own sentence.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// src/extension.ts shows the notice, runs its one action and writes the
// details, and decides nothing.

import type { ConnectionState } from "../connection/manager.js";
import { compareVersions } from "./compare.js";

export type LanguageSkewState = "unknown" | "match" | "clusterNewer" | "clusterOlder" | "differs";

/** One side's language. The cluster fills the first three, this extension the other. */
export interface LanguageFacts {
  /** The edition, e.g. "2026". */
  edition?: string;
  /** The grammar version inside the edition. A label: compared for equality only. */
  grammarVersion?: string;
  /** The cluster's: the first release of MemQL for VS Code that carries its grammar. */
  editorRelease?: string;
  /** This extension's own version, from its package.json. */
  version?: string;
}

export interface LanguageSkew {
  state: LanguageSkewState;
  /** The notice's sentence. Empty exactly when nothing is shown: unknown and match. */
  headline: string;
  /** For the output channel: both sides, and the release that carries the cluster's grammar. */
  details: string[];
  /** The release to install. Set only when the cluster is newer and named one. */
  releaseToInstall?: string;
}

/** The id `extension.open` takes to open this extension's page in the Extensions view. */
export const MEMQL_EXTENSION_ID = "znasllc.memql";

/** The warning's own action. */
export const OPEN_IN_EXTENSIONS = "Open in Extensions";

/** What a notice without an order can still say about its consequence. */
const MAY_NOT_MATCH = "Completion and diagnostics may not match the cluster.";

/** Editions are years, so two of them order as numbers -- when both are years. */
const EDITION = /^\d{4}$/;

function clean(value: string | undefined): string {
  return (value ?? "").trim();
}

function compareEditions(a: string, b: string): number | undefined {
  if (!EDITION.test(a) || !EDITION.test(b)) return undefined;
  return Number(a) - Number(b);
}

interface ClusterSide {
  edition: string;
  grammar: string;
  release: string;
}

interface ExtensionSide {
  edition: string;
  grammar: string;
  version: string;
}

function detailLines(c: ClusterSide, e: ExtensionSide): string[] {
  const cluster =
    c.edition === ""
      ? "The cluster reported no MemQL edition, so its language cannot be compared with this extension's."
      : c.grammar === ""
        ? `The cluster speaks MemQL edition ${c.edition} and reported no grammar version.`
        : `The cluster speaks MemQL edition ${c.edition}, grammar ${c.grammar}.`;
  const self = e.version === "" ? "This extension" : `This extension (MemQL for VS Code ${e.version})`;
  const extension =
    e.edition === ""
      ? `${self} carries no language pin, so the cluster's language cannot be compared with it.`
      : e.grammar === ""
        ? `${self} speaks edition ${e.edition} and pins no grammar version.`
        : `${self} speaks edition ${e.edition}, grammar ${e.grammar}.`;
  const release =
    c.release === ""
      ? "The cluster did not name the release of MemQL for VS Code that carries its grammar."
      : `MemQL for VS Code ${c.release} is the first release that carries the cluster's grammar.`;
  return [cluster, extension, release];
}

/**
 * How the cluster's language relates to this extension's.
 *
 * `cluster` is what the handshake stated; `extension` is this build's
 * package.json. Whitespace around a value is not a difference.
 */
export function compareLanguage(cluster: LanguageFacts, extension: LanguageFacts): LanguageSkew {
  const c: ClusterSide = {
    edition: clean(cluster.edition),
    grammar: clean(cluster.grammarVersion),
    release: clean(cluster.editorRelease),
  };
  const e: ExtensionSide = {
    edition: clean(extension.edition),
    grammar: clean(extension.grammarVersion),
    version: clean(extension.version),
  };
  const details = detailLines(c, e);

  // Nothing to compare. A cluster older than the fields states "" for each,
  // and a build with no pin has no side of its own.
  if (c.edition === "" || e.edition === "") return { state: "unknown", headline: "", details };

  if (c.edition !== e.edition) {
    const both = `This cluster speaks MemQL edition ${c.edition}; this extension speaks edition ${e.edition}.`;
    const order = compareEditions(c.edition, e.edition);
    if (order !== undefined && order > 0) {
      // The release comes from the cluster. One that did not name it leaves
      // the notice naming the edition to get rather than inventing a version.
      if (c.release === "") {
        return {
          state: "clusterNewer",
          headline: `${both} Update MemQL for VS Code to a release that speaks edition ${c.edition}.`,
          details,
        };
      }
      return {
        state: "clusterNewer",
        headline: `${both} Update MemQL for VS Code to ${c.release} or newer.`,
        details,
        releaseToInstall: c.release,
      };
    }
    if (order !== undefined && order < 0) {
      return {
        state: "clusterOlder",
        headline: `${both} The editor may suggest forms this cluster refuses.`,
        details,
      };
    }
    return {
      state: "differs",
      headline: `This cluster speaks MemQL edition ${c.edition} and this extension speaks edition ${e.edition}, and which is newer cannot be shown. ${MAY_NOT_MATCH}`,
      details,
    };
  }

  // One edition. A grammar missing on either side cannot be a match.
  if (c.grammar === "" || e.grammar === "") return { state: "unknown", headline: "", details };
  if (c.grammar === e.grammar) return { state: "match", headline: "", details };

  // Different grammars. Their labels do not order; the release that first
  // carried the cluster's grammar does, against this extension's version.
  switch (compareVersions(e.version, c.release)) {
    case "behind":
      return {
        state: "clusterNewer",
        headline: `This cluster's MemQL grammar is newer than this extension's. Update MemQL for VS Code to ${c.release} or newer so completion and diagnostics match the cluster.`,
        details,
        releaseToInstall: c.release,
      };
    case "ahead":
      return {
        state: "clusterOlder",
        headline:
          "This cluster's MemQL grammar is older than this extension's. The editor may suggest forms this cluster refuses.",
        details,
      };
    default:
      // "current" -- the cluster's grammar first shipped in this very release
      // number, yet the grammars differ: a locally built extension -- or a
      // release that does not parse. Either way, no order.
      return {
        state: "differs",
        headline: `This cluster's MemQL grammar (${c.grammar}) differs from this extension's (${e.grammar}), and which is newer cannot be shown. ${MAY_NOT_MATCH}`,
        details,
      };
  }
}

/** A notice to show: how loud, what it says, and what it offers. */
export interface LanguageSkewNotice {
  severity: "warning" | "information";
  headline: string;
  /**
   * This notice's own actions, in order. "Show details" is appended after
   * them by the toast helper in src/extension.ts, on every notice.
   */
  actions: string[];
  details: string[];
}

/**
 * The notice for a comparison, or undefined when there is nothing to say.
 *
 * Only a newer cluster is a warning, and only it offers an action beyond the
 * details: the fix is on this side -- install the release it names. An older
 * cluster, or one that cannot be ordered, is information.
 */
export function languageSkewNotice(skew: LanguageSkew): LanguageSkewNotice | undefined {
  switch (skew.state) {
    case "clusterNewer":
      return { severity: "warning", headline: skew.headline, actions: [OPEN_IN_EXTENSIONS], details: skew.details };
    case "clusterOlder":
    case "differs":
      return { severity: "information", headline: skew.headline, actions: [], details: skew.details };
    default:
      return undefined;
  }
}

/**
 * Which clusters, on which grammar, this session has already been told about.
 *
 * ONCE PER CLUSTER AND GRAMMAR, PER SESSION, in memory -- the shape
 * OfferMemory (src/auth/passkeyOffer.ts) takes for the same reason. A
 * reconnect to the same cluster is not news, and a notice that returns on
 * every reconnect becomes one nobody reads. The same cluster on a NEW grammar
 * is news: it was upgraded under this editor. And a new window asks again,
 * because the facts may have changed while it was closed.
 */
export class LanguageSkewMemory {
  private readonly seen = new Set<string>();

  /** True the first time this cluster is seen on this edition and grammar; false after. */
  firstTime(clusterName: string, cluster: LanguageFacts): boolean {
    const key = JSON.stringify([clusterName, clean(cluster.edition), clean(cluster.grammarVersion)]);
    if (this.seen.has(key)) return false;
    this.seen.add(key);
    return true;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringOrUndefined(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

/**
 * This extension's side, read from its own package.json
 * (`context.extension.packageJSON`): the `memql` pin and the version.
 *
 * Anything that is not a string reads as absent, and absent compares as
 * unknown -- the fake contexts the activation tests build carry no
 * `extension` at all, and a manifest without the pin must not look like a
 * mismatch.
 */
export function extensionLanguageFacts(packageJSON: unknown): LanguageFacts {
  const manifest = isRecord(packageJSON) ? packageJSON : {};
  const pin = isRecord(manifest["memql"]) ? manifest["memql"] : {};
  return {
    edition: stringOrUndefined(pin["edition"]),
    grammarVersion: stringOrUndefined(pin["grammarVersion"]),
    version: stringOrUndefined(manifest["version"]),
  };
}

/** What the watcher needs from a connection: its state changes and the language it stated. */
export interface LanguageSource {
  onDidChangeState(listener: (state: ConnectionState) => void): () => void;
  readonly edition: string | undefined;
  readonly grammarVersion: string | undefined;
  readonly editorRelease: string | undefined;
}

/**
 * Compares the languages every time `source` connects, and hands each notice
 * to `present` at most once per cluster and grammar. Returns the function that
 * stops watching.
 *
 * ONE LISTENER COVERS EVERY CONNECT PATH. Selecting a cluster, the reconnect
 * after sign-in and an opened `vscode://` link all end in the manager
 * publishing "connected"; hooking one call site would cover one of them.
 *
 * `readExtension` is read at each connect rather than once at wiring, so the
 * answer is always this build's manifest as the editor currently reports it.
 */
export function watchLanguageSkew(
  source: LanguageSource,
  readExtension: () => LanguageFacts,
  present: (notice: LanguageSkewNotice, clusterName: string) => void,
  memory: LanguageSkewMemory = new LanguageSkewMemory(),
): () => void {
  return source.onDidChangeState((state) => {
    if (state.status !== "connected") return;
    const cluster: LanguageFacts = {
      edition: source.edition,
      grammarVersion: source.grammarVersion,
      editorRelease: source.editorRelease,
    };
    const notice = languageSkewNotice(compareLanguage(cluster, readExtension()));
    if (notice === undefined) return;
    if (!memory.firstTime(state.clusterName, cluster)) return;
    present(notice, state.clusterName);
  });
}

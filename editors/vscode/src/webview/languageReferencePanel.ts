// The language reference tab: the cluster's grammar and vocabulary, in the
// editor.
//
// WHAT THIS FILE IS ALLOWED TO DECIDE: when to ask, what to do with a message,
// and what goes on the clipboard. Everything else -- which language the page is
// showing, how a builtin reply narrows, how the grammar splits into
// productions, what a search matches -- lives in state/languageReference.ts
// and webview/languageReferenceScreens.ts, under bare `node --test`.
//
// IT IS NOT GATED ON WORKSPACE TRUST, and that is the one unusual thing about
// it. Every other panel here reads a credential or opens a connection and is
// registered by registerRuntimeSurface; this one is a LANGUAGE surface. With
// no cluster it shows the edition and grammar version the extension was built
// against -- which is exactly the language its own server is giving the person
// completion and diagnostics in -- and it reads no credential to do that. A
// restricted folder is precisely where somebody is most likely to be reading
// `.memql` with nothing connected.
//
// THE FETCH IS GUARDED but the panel is a singleton, so a reconnect to another
// cluster while a read is in flight must not land the old cluster's grammar
// under the new cluster's name. `Latest` is the shared guard the rest of the
// extension's async paths use.
//
// Refs: memql#5388 (the two builtins), memql#5362 (the pin this reads).

import { randomBytes } from "node:crypto";

import * as vscode from "vscode";

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { brandStrip, brandStyleBlock } from "./brandTokens.js";
import { currentBodyThemeAttr, onAppearanceChange } from "./theme.js";
import {
  COPY_GRAMMAR,
  COPY_VOCABULARY,
  RELOAD,
  SEARCH_MESSAGE,
  SELECT_CLUSTER,
  languageReferenceScript,
  languageReferenceStyles,
  renderLanguageReferencePage,
} from "./languageReferenceScreens.js";

import { Latest } from "../async/latest.js";
import {
  GRAMMAR_CALL,
  GRAMMAR_NAME,
  VOCABULARY_CALL,
  VOCABULARY_NAME,
  clusterLanguage,
  grammarFromRows,
  languageIdentity,
  vocabularyFromRows,
  vocabularyText,
  type ClusterLanguage,
  type GrammarArtifact,
  type LanguagePin,
  type VocabularyArtifact,
} from "../state/languageReference.js";

/** The command that opens this panel. Spelled once; the manifest's twin is gated by a test. */
export const COMMAND_LANGUAGE_REFERENCE = "memql.language.showReference";

/** The panel's title, and the tab label. */
const TITLE = "MemQL language reference";

/**
 * How long the two reads are given before the page gives up on them.
 *
 * A DEADLINE IS NOT OPTIONAL HERE, because this page promises in so many words
 * that it is never a spinner that never ends. `Dispatcher.sendAndWait` has no
 * deadline of its own: a cluster that accepts the call and never answers leaves
 * a promise that never settles, and `retryHtml` withholds "Try again" while a
 * read is in flight -- so without this the one path that breaks the page's own
 * claim is the one nobody can act on.
 *
 * THE RIGHT LONG-TERM HOME IS THE SDK, not this file. Every caller of
 * `executeNamed` has the same exposure, and `QueryCallOptions.signal` is the
 * seam a default deadline would be built on. Until somebody decides that, the
 * page that makes the promise keeps it.
 *
 * Twenty seconds because both builtins render in-memory tables -- they are slow
 * only if something is wrong -- and because a deadline a healthy cluster can
 * trip is a deadline that teaches people to ignore the message.
 */
export const READ_DEADLINE_MS = 20_000;

/**
 * The slice of a live connection this panel reads.
 *
 * STRUCTURAL rather than the ConnectionManager itself, for the reason
 * library/artifactMeta.ts gives for its own: a narrow shape can be faked in a
 * unit test, and this panel's whole behaviour with and without a cluster is
 * exactly what wants testing.
 */
export interface LanguageClusterReader {
  /** The cluster this reader speaks for, for the sentence naming it. */
  name: string;
  /** The edition it stated on the handshake, if it stated one. */
  edition: string | undefined;
  /** The grammar version it stated on the handshake, if it stated one. */
  grammarVersion: string | undefined;
  /**
   * Run one call. `options.signal` is the SDK's own
   * `QueryCallOptions.signal`, and it is what the read deadline is built on --
   * `Dispatcher.sendAndWait` drops the pending entry and rejects when it fires.
   */
  executeNamed(
    name: string,
    call: string,
    options?: { signal?: AbortSignal },
  ): Promise<{ rows(): Record<string, unknown>[] }>;
}

/** What the panel needs from the host. */
export interface LanguageReferenceDeps {
  /** This extension's own language pin, from its package.json. */
  pin: LanguagePin;
  /**
   * The connected cluster, or undefined when there is none to ask.
   *
   * A FUNCTION, re-read on every load: a panel left open across a sign-out and
   * a reconnect must ask whoever is connected now, and a reader captured at
   * construction would go on reading a dead stream.
   */
  reader: () => LanguageClusterReader | undefined;
  /**
   * Whether this window can offer to pick a cluster -- that is, whether the
   * runtime surface is registered. False in an untrusted workspace, where
   * `memql.clusters.select` is contributed but never registered, so offering
   * the button would promise a click that fails with "command not found".
   */
  canSelectCluster: () => boolean;
  /**
   * Subscribe to connection changes. Returns the unsubscribe.
   *
   * CALLED AGAIN ON EVERY `open()`, not once at construction. In an untrusted
   * window there is no ConnectionManager to subscribe to, so this returns a
   * no-op unsubscribe over nothing -- and a panel that bound that once was
   * deaf to every connect, disconnect and switch for the rest of its life,
   * including after the workspace was trusted. Re-establishing it is what
   * makes REFERENCE.md's claim true rather than nearly true.
   */
  onDidChangeConnection?: (listener: () => void) => () => void;
  /**
   * Override the read deadline. Present for the tests, which cannot wait
   * READ_DEADLINE_MS to watch one expire; every real caller omits it.
   */
  readDeadlineMs?: number;
}

export class LanguageReferencePanel {
  private static open_: LanguageReferencePanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly disposables: vscode.Disposable[] = [];
  private readonly latest = new Latest<"languageReference">();

  private cluster: ClusterLanguage | undefined;
  private grammar: GrammarArtifact | undefined;
  private vocabulary: VocabularyArtifact | undefined;
  private loading = false;
  private error = "";
  /**
   * The search term the page currently holds.
   *
   * Kept ONLY so a repaint can seed the box again -- a theme flip or a
   * reconnect replaces the whole document, and losing what somebody was
   * halfway through typing is the kind of small rudeness that makes a surface
   * feel broken. Nothing re-renders because this changed.
   */
  private search = "";
  /** Unsubscribe for the connection listener, replaced whenever it is re-bound. */
  private unsubscribeConnection: (() => void) | undefined;
  private disposed = false;

  static open(
    context: vscode.ExtensionContext,
    deps: LanguageReferenceDeps,
  ): LanguageReferencePanel {
    const existing = LanguageReferencePanel.open_;
    if (existing !== undefined && !existing.disposed) {
      existing.panel.reveal(vscode.ViewColumn.Beside);
      // Re-pointed at THIS opener's deps, the way ConstructPanel is: the panel
      // outlives the call that made it, and the host it was first opened from
      // may since have grown a connection it did not have.
      existing.deps = deps;
      // AND RE-SUBSCRIBED, which re-pointing the deps alone does not do. The
      // first open may have bound over a host with no ConnectionManager -- an
      // untrusted window -- and that subscription is a no-op forever. Without
      // this, the panel reloads once on re-open and is then deaf to every
      // later connect, disconnect and cluster switch.
      existing.subscribe();
      void existing.load();
      return existing;
    }
    const panel = new LanguageReferencePanel(context, deps);
    LanguageReferencePanel.open_ = panel;
    return panel;
  }

  /**
   * Re-bind and re-read an OPEN panel, for a host whose connection surface has
   * only just appeared.
   *
   * The one caller is src/extension.ts's workspace-trust listener, which is the
   * moment a window that could not connect becomes one that can. The panel
   * predates that surface -- it is registered outside the trust gate on
   * purpose -- so nothing else is in a position to tell it. A no-op when no
   * panel is open, which is every other time it is called.
   */
  static connectionSurfaceChanged(): void {
    const panel = LanguageReferencePanel.open_;
    if (panel === undefined || panel.disposed) return;
    panel.subscribe();
    void panel.load();
  }

  private constructor(
    _context: vscode.ExtensionContext,
    private deps: LanguageReferenceDeps,
  ) {
    this.panel = vscode.window.createWebviewPanel(
      "memqlLanguageReference",
      TITLE,
      vscode.ViewColumn.Beside,
      { enableScripts: true, retainContextWhenHidden: true },
    );
    this.disposables.push(
      // The palette is a MemQL setting, not the editor's theme, so an OPEN
      // panel repaints when either input moves (memql#4419).
      ...onAppearanceChange(() => this.render()),
      this.panel.onDidDispose(() => {
        this.disposed = true;
        this.latest.invalidate();
        this.unsubscribeConnection?.();
        this.unsubscribeConnection = undefined;
        if (LanguageReferencePanel.open_ === this) LanguageReferencePanel.open_ = undefined;
        for (const d of this.disposables.splice(0)) d.dispose();
      }),
      this.panel.webview.onDidReceiveMessage((message: unknown) => {
        void this.onMessage(message);
      }),
    );
    this.subscribe();

    this.render();
    void this.load();
  }

  /**
   * Bind the connection listener, dropping whatever was bound before.
   *
   * Held in a field rather than on `this.disposables`, because that bag is
   * disposed once with the tab and this subscription is replaced several times
   * before then. Dropping the old one first is what keeps a re-open from
   * leaving two listeners both reloading the same panel.
   */
  private subscribe(): void {
    this.unsubscribeConnection?.();
    this.unsubscribeConnection = this.deps.onDidChangeConnection?.(() => void this.load());
  }

  /**
   * Read both artifacts from whoever is connected, or say there is nobody.
   *
   * BOTH CALLS ARE MADE, and a failure of one does not cancel the other: they
   * are separate builtins and a cluster that answers one and refuses the other
   * has told the reader something. The page renders whichever arrived and says
   * so for the one that did not.
   */
  private async load(): Promise<void> {
    const reader = this.deps.reader();
    const token = this.latest.begin();
    if (reader === undefined) {
      this.cluster = undefined;
      this.grammar = undefined;
      this.vocabulary = undefined;
      this.loading = false;
      this.error = "";
      this.render();
      return;
    }

    // The cluster's own handshake facts go up IMMEDIATELY, so the header names
    // the language before the artifacts arrive rather than sitting blank
    // behind a spinner.
    this.cluster = clusterLanguage(reader.name, reader);
    this.grammar = undefined;
    this.vocabulary = undefined;
    this.loading = true;
    this.error = "";
    this.render();

    // THE DEADLINE. One controller for both calls: they are asked together and
    // a cluster that has stopped answering has stopped answering both. `expired`
    // is what turns the SDK's abort rejection -- which says only "aborted" --
    // into the sentence that names the call and the time it was given.
    const deadlineMs = this.deps.readDeadlineMs ?? READ_DEADLINE_MS;
    const deadline = new AbortController();
    let expired = false;
    const timer = setTimeout(() => {
      expired = true;
      deadline.abort();
    }, deadlineMs);
    const options = { signal: deadline.signal };

    const [grammar, vocabulary] = await Promise.all([
      reader
        .executeNamed(GRAMMAR_NAME, GRAMMAR_CALL, options)
        .then((result) => grammarFromRows(result.rows()))
        .catch((err: unknown) => (expired ? timedOut(GRAMMAR_NAME, deadlineMs) : failure(GRAMMAR_NAME, err))),
      reader
        .executeNamed(VOCABULARY_NAME, VOCABULARY_CALL, options)
        .then((result) => vocabularyFromRows(result.rows()))
        .catch((err: unknown) =>
          expired ? timedOut(VOCABULARY_NAME, deadlineMs) : failure(VOCABULARY_NAME, err),
        ),
    ]);
    clearTimeout(timer);
    // Superseded by a reconnect, a re-open or the tab closing while both calls
    // were in flight. Writing here would put one cluster's grammar under
    // another cluster's name.
    if (this.disposed || !this.latest.isCurrent(token)) return;

    this.loading = false;
    this.grammar = typeof grammar === "string" ? undefined : grammar;
    this.vocabulary = typeof vocabulary === "string" ? undefined : vocabulary;
    this.error = [grammar, vocabulary].filter((v): v is string => typeof v === "string").join(" ");
    // The artifacts state the edition and grammar version they were RENDERED
    // from, which is the pair the header should name once they are on screen.
    this.cluster = clusterLanguage(reader.name, reader, this.grammar, this.vocabulary);
    this.render();
  }

  private async onMessage(message: unknown): Promise<void> {
    if (message === null || typeof message !== "object") return;
    const { type, term } = message as { type?: unknown; term?: unknown };
    if (type === SEARCH_MESSAGE) {
      // Remembered, never acted on: see the field's comment.
      if (typeof term === "string") this.search = term;
      return;
    }
    if (type === COPY_GRAMMAR) {
      await this.copy(
        this.grammar?.content,
        "The grammar is on the clipboard.",
        "There is no grammar to copy: no cluster has answered with one.",
      );
      return;
    }
    if (type === COPY_VOCABULARY) {
      await this.copy(
        this.vocabulary === undefined ? undefined : vocabularyText(this.vocabulary),
        "The vocabulary is on the clipboard.",
        "There is no vocabulary to copy: no cluster has answered with one.",
      );
      return;
    }
    if (type === SELECT_CLUSTER) {
      await vscode.commands.executeCommand("memql.clusters.select");
      return;
    }
    if (type === RELOAD) {
      await this.load();
    }
  }

  /**
   * Put an artifact on the clipboard, or say why there is nothing to put
   * there.
   *
   * The refusal is a MESSAGE rather than a silent no-op. The buttons are drawn
   * whenever either artifact is on the page, so pressing the other one is an
   * ordinary thing to do, and a copy that quietly does nothing is
   * indistinguishable from one that worked -- with the difference discovered
   * at the paste.
   */
  private async copy(value: string | undefined, done: string, missing: string): Promise<void> {
    if (value === undefined || value === "") {
      void vscode.window.showWarningMessage(`MemQL: ${missing}`);
      return;
    }
    await vscode.env.clipboard.writeText(value);
    void vscode.window.showInformationMessage(`MemQL: ${done}`);
  }

  private render(): void {
    if (this.disposed) return;
    const nonce = nonceValue();
    const body = renderLanguageReferencePage({
      identity: languageIdentity({ pin: this.deps.pin, cluster: this.cluster }),
      // Handed over for the one case it is read in: a cluster was asked and
      // answered with neither artifact, where the identity block above is
      // describing the cluster and the reader would otherwise learn nothing
      // about the language their editor is giving them right now.
      pin: this.deps.pin,
      grammar: this.grammar,
      vocabulary: this.vocabulary,
      loading: this.loading,
      error: this.error,
      search: this.search,
      offerSelectCluster: this.deps.canSelectCluster(),
    });
    this.panel.webview.html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy"
      content="default-src 'none'; style-src 'nonce-${nonce}'; script-src 'nonce-${nonce}';">
<title>${escapeHtml(TITLE)}</title>
<style nonce="${nonce}">
${brandStyleBlock()}
${languageReferenceStyles()}
</style>
</head>
<body${currentBodyThemeAttr()}>
${brandStrip("MemQL for Visual Studio Code and Cursor")}
${body}
<script nonce="${nonce}">${languageReferenceScript()}</script>
</body>
</html>`;
  }
}

/**
 * One call's failure, as the sentence the page prints.
 *
 * It names the BUILTIN, because the two are separate calls with separate
 * reasons to fail -- a cluster too old to carry one of them refuses it by name,
 * and "the language reference failed" would leave a reader with no way to tell
 * that from a dropped connection.
 */
function failure(name: string, err: unknown): string {
  return `${name}() could not be read: ${err instanceof Error ? err.message : String(err)}`;
}

/**
 * The deadline's own sentence.
 *
 * SEPARATE FROM `failure` because the SDK's abort rejection says only
 * "aborted", which reads as something the reader did. This says what actually
 * happened: the cluster took the call and did not answer inside the time it was
 * given. The page then shows what the extension knows and offers Try again,
 * which is the whole reason the deadline exists.
 */
function timedOut(name: string, deadlineMs: number): string {
  const seconds = Math.round(deadlineMs / 1000);
  const time = seconds >= 1 ? `${seconds}s` : `${deadlineMs}ms`;
  return `${name}() did not answer within ${time}, so the read was given up. The cluster may still be working on it.`;
}

/** A CSP nonce, from a CSPRNG: a predictable one is one an injection can carry. */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}

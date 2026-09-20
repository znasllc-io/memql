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
  executeNamed(name: string, call: string): Promise<{ rows(): Record<string, unknown>[] }>;
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
  /** Subscribe to connection changes. Returns the unsubscribe. */
  onDidChangeConnection?: (listener: () => void) => () => void;
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
      void existing.load();
      return existing;
    }
    const panel = new LanguageReferencePanel(context, deps);
    LanguageReferencePanel.open_ = panel;
    return panel;
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
        if (LanguageReferencePanel.open_ === this) LanguageReferencePanel.open_ = undefined;
        for (const d of this.disposables.splice(0)) d.dispose();
      }),
      this.panel.webview.onDidReceiveMessage((message: unknown) => {
        void this.onMessage(message);
      }),
    );
    const unsubscribe = this.deps.onDidChangeConnection?.(() => void this.load());
    if (unsubscribe !== undefined) this.disposables.push({ dispose: unsubscribe });

    this.render();
    void this.load();
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

    const [grammar, vocabulary] = await Promise.all([
      reader
        .executeNamed(GRAMMAR_NAME, GRAMMAR_CALL)
        .then((result) => grammarFromRows(result.rows()))
        .catch((err: unknown) => failure(GRAMMAR_NAME, err)),
      reader
        .executeNamed(VOCABULARY_NAME, VOCABULARY_CALL)
        .then((result) => vocabularyFromRows(result.rows()))
        .catch((err: unknown) => failure(VOCABULARY_NAME, err)),
    ]);
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
    if (type === "reload") {
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

/** A CSP nonce, from a CSPRNG: a predictable one is one an injection can carry. */
function nonceValue(): string {
  return randomBytes(16).toString("base64");
}

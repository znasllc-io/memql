// The CodeLens adapter: ask the language server which constructs are runnable,
// render an affordance on each signature.
//
// An ADAPTER, in the sense the no-vscode-import rule means it: it holds no
// logic of its own. The decoding lives in constructs/runnable.ts and the lens
// set comes out of lensPlansFor(); this file converts LSP ranges to
// vscode.Range, LSP requests to a client call, and nothing else.
//
// NOTHING HERE EXECUTES ANYTHING. provideCodeLenses runs on open, on every
// edit, and whenever VS Code feels like refreshing -- so it must be free of
// side effects beyond one read-only LSP request. The commands the lenses carry
// fire only on a click. There is no run-on-open and no run-on-save anywhere in
// this extension, which is what makes it safe for a repository to ship a run
// configuration.

import * as vscode from "vscode";

import {
  COMMAND_RUN_NOT_AVAILABLE,
  RUNNABLE_CONSTRUCTS_CAPABILITY,
  RUNNABLE_CONSTRUCTS_METHOD,
  lensPlansFor,
  parseRunnableConstructs,
  type LspRange,
  type RunnableConstruct,
} from "./runnable.js";

// The slice of vscode-languageclient's LanguageClient this provider needs.
//
// Declared structurally rather than imported so this file's dependency is the
// CAPABILITY, not the package: the provider works against anything that can
// send a request and report what the server advertised, which is also what
// makes the feature-detection testable without booting a language server.
export interface RunnableConstructsClient {
  sendRequest(method: string, params: unknown, token?: vscode.CancellationToken): Promise<unknown>;
  /** The server's advertised `capabilities.experimental`, or undefined before initialize. */
  experimentalCapabilities(): Record<string, unknown> | undefined;
}

export class RunnableCodeLensProvider implements vscode.CodeLensProvider {
  private readonly changed = new vscode.EventEmitter<void>();
  readonly onDidChangeCodeLenses = this.changed.event;

  constructor(private readonly client: RunnableConstructsClient) {}

  /** Forces a refresh -- used when the server (re)starts, so lenses appear without an edit. */
  refresh(): void {
    this.changed.fire();
  }

  async provideCodeLenses(
    document: vscode.TextDocument,
    token: vscode.CancellationToken,
  ): Promise<vscode.CodeLens[]> {
    // FEATURE-DETECT rather than calling blind. An older memql-lsp on the PATH
    // is an ordinary situation -- serverPath is a user setting and the PATH
    // fallback is whatever is installed -- and a MethodNotFound surfacing as a
    // popup on every keystroke would be intolerable.
    const experimental = this.client.experimentalCapabilities();
    if (experimental === undefined || experimental[RUNNABLE_CONSTRUCTS_CAPABILITY] !== true) {
      return [];
    }

    let raw: unknown;
    try {
      raw = await this.client.sendRequest(
        RUNNABLE_CONSTRUCTS_METHOD,
        { textDocument: { uri: document.uri.toString() } },
        token,
      );
    } catch {
      // A cancelled or failed request is not an error state to render. VS Code
      // cancels a CodeLens request on the next keystroke as a matter of course,
      // so this path is hit constantly during ordinary typing.
      return [];
    }
    if (token.isCancellationRequested) return [];

    const constructs: RunnableConstruct[] = parseRunnableConstructs(raw);
    return lensPlansFor(document.uri.toString(), constructs).map((plan) => {
      // The command is attached HERE rather than left for resolveCodeLens: a
      // lens with no command is "unresolved", and VS Code renders it as a
      // placeholder until a resolveCodeLens this provider does not implement
      // fills it in. Everything is already in hand, so there is nothing to
      // defer.
      const command: vscode.Command = {
        title: plan.title,
        command: plan.command,
        tooltip: plan.tooltip,
        arguments: plan.command === COMMAND_RUN_NOT_AVAILABLE ? [plan.reason] : [plan.target],
      };
      return new vscode.CodeLens(toRange(plan.range), command);
    });
  }
}

function toRange(range: LspRange): vscode.Range {
  return new vscode.Range(
    range.start.line,
    range.start.character,
    range.end.line,
    range.end.character,
  );
}

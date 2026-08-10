// A hand-written stand-in for the editor-injected `vscode` module, used by the
// activation cases (memql#3387) and by the add-a-cluster panel (memql#3514).
//
// WHY IT EXISTS. Every other module in this package that carries logic is free
// of `vscode` imports -- that is what lets the fast lane run under bare
// `node --test` with no Electron, and it is enforced mechanically by
// cmd/memql-lsp/vscodeimportrule_test.go. src/extension.ts is on that guard's
// allow-list as an ADAPTER, and adapters are normally left to the host smoke
// lane. activate() is the exception: its whole content is the ORDER in which
// two independent surfaces are brought up, one of which was skipping the other
// entirely, and an ordering defect is exactly what a unit test catches and a
// smoke lane does not. Driving the real activate() needs a `vscode`, so here is
// one.
//
// HOW IT IS WIRED. esbuild.test.js aliases the bare specifier `vscode` to this
// file when it bundles test/*.test.ts, so src/extension.ts and the test import
// the SAME module instance and the test can read what activation did. tsc still
// type-checks the extension against the real @types/vscode; this file is a
// runtime substitution only and deliberately does not implement vscode.d.ts.
//
// WHAT IT COVERS. The activation path, plus the WEBVIEW PANEL surface
// src/webview/addClusterPanel.ts drives (memql#3514). A member reached only
// from a command handler or a tree item render is absent on purpose: an absent
// member fails loudly the moment a case wanders past what this stub claims to
// model, which is the right outcome for a fake.
//
// WHY THE WEBVIEW SURFACE IS HERE AT ALL. `AddClusterPanel` is where the state
// machine, the session layer, the graph and the workbench meet, and it was the
// only layer of that stack with no test of any kind -- which is where four
// defects reached main during epic #3463, each satisfied by something ADJACENT
// to the requirement (a type existing, a state transition happening, a button
// rendering) while the thing the operator needed did not happen. The host smoke
// lane structurally cannot reach it: a card is a `<button data-choose=...>`
// inside the webview's iframe and nothing host-side can dispatch a DOM event
// into it. So the page's own script is the ONE part modelled by hand here --
// `send()` posts what a click would post -- and everything below it, message
// handling included, is the real panel.

export interface StubDisposable {
  dispose(): void;
}

/** The `inspect()` shape src/extension.ts reads for memql.lsp.serverPath. */
export interface ConfigurationValues {
  defaultValue?: string;
  globalValue?: string;
  workspaceValue?: string;
  workspaceFolderValue?: string;
}

/** What activation did, in the order it did it. */
export const recorded = {
  /** window.showErrorMessage bodies. */
  errors: [] as string[],
  /** window.showWarningMessage bodies. */
  warnings: [] as string[],
  /** window.showInformationMessage bodies. */
  infos: [] as string[],
  /** View ids passed to window.registerTreeDataProvider. */
  treeViews: [] as string[],
  /** Command ids passed to commands.registerCommand. */
  commands: [] as string[],
  /** File-system watcher globs, as `<base>/<pattern>`. */
  watched: [] as string[],
  /** Command ids passed to commands.executeCommand, in order. */
  executed: [] as string[],
  /** Every panel window.createWebviewPanel has produced, in order. */
  webviews: [] as StubWebviewPanel[],
};

/** Drops everything `recorded` holds. Call between cases. */
export function resetRecorded(): void {
  recorded.errors.length = 0;
  recorded.warnings.length = 0;
  recorded.infos.length = 0;
  recorded.treeViews.length = 0;
  recorded.commands.length = 0;
  recorded.watched.length = 0;
  recorded.executed.length = 0;
  recorded.webviews.length = 0;
}

/**
 * Settings the stubbed `workspace.getConfiguration(section).inspect(key)`
 * answers with, keyed `<section>.<key>`. An absent key inspects to undefined,
 * which is what VS Code reports for a setting nobody has ever written.
 */
export const settings = new Map<string, ConfigurationValues>();

// The one-shot workspace-trust listener activate() arms in a restricted
// window. Held here so a case can grant trust and observe the handover.
let trustListener: (() => void) | undefined;

/** Fires onDidGrantWorkspaceTrust, as clicking "Trust This Workspace" does. */
export function grantWorkspaceTrust(): void {
  trustListener?.();
}

/** Reports whether the one-shot trust listener is currently armed. */
export function isTrustListenerArmed(): boolean {
  return trustListener !== undefined;
}

// -----------------------------------------------------------------------------
// The `vscode` surface
// -----------------------------------------------------------------------------

export class EventEmitter<T> {
  private readonly listeners = new Set<(value: T) => void>();

  readonly event = (listener: (value: T) => void): StubDisposable => {
    this.listeners.add(listener);
    return {
      dispose: () => {
        this.listeners.delete(listener);
      },
    };
  };

  fire(value: T): void {
    for (const listener of [...this.listeners]) listener(value);
  }

  dispose(): void {
    this.listeners.clear();
  }
}

/**
 * The disposable VS Code exposes as a CLASS, not just as an interface.
 *
 * `AddClusterPanel` constructs one (`new vscode.Disposable(() => ...)`) to hang
 * its own teardown off the extension context, so the constructor form has to
 * exist rather than only the shape.
 */
export class Disposable implements StubDisposable {
  constructor(private readonly onDispose: () => void) {}

  dispose(): void {
    this.onDispose();
  }
}

export const ViewColumn = {
  Active: -1,
  Beside: -2,
  One: 1,
  Two: 2,
} as const;

/**
 * A webview panel, as a test drives it.
 *
 * `html` is what the extension last rendered -- the ONE thing worth asserting
 * about a webview, since it is the entirety of what the operator can see and
 * act on. `send()` is the direction the real host API does not offer: it plays
 * the part of the page's own click handler, posting the message a
 * `data-act`/`data-choose` button would post.
 */
export interface StubWebviewPanel {
  viewType: string;
  title: string;
  /** The last HTML the extension assigned. Empty before the first render. */
  html: string;
  /** Posts a message from the PAGE to the extension, as a click would. */
  send(message: unknown): void;
  /** How many times the extension asked to bring this panel forward. */
  revealCount: number;
  disposed: boolean;
  /** Closes the panel from the EDITOR's side, as clicking the tab's x does. */
  close(): void;
}

/** The object handed to the extension. Its `html` writes land on the stub. */
interface WebviewPanelSurface {
  webview: {
    html: string;
    onDidReceiveMessage(handler: (message: unknown) => void): StubDisposable;
    postMessage(message: unknown): Promise<boolean>;
  };
  reveal(column?: number): void;
  onDidDispose(handler: () => void, thisArg?: unknown, disposables?: StubDisposable[]): StubDisposable;
  dispose(): void;
}

function createStubWebviewPanel(viewType: string, title: string): {
  handle: StubWebviewPanel;
  surface: WebviewPanelSurface;
} {
  const inbound: ((message: unknown) => void)[] = [];
  const onDispose: (() => void)[] = [];

  const handle: StubWebviewPanel = {
    viewType,
    title,
    html: '',
    revealCount: 0,
    disposed: false,
    send(message: unknown): void {
      for (const listener of [...inbound]) listener(message);
    },
    close(): void {
      surface.dispose();
    },
  };

  const surface: WebviewPanelSurface = {
    webview: {
      // The extension assigns `panel.webview.html`; the assignment is the whole
      // render, so it is captured on the handle rather than kept here.
      get html(): string {
        return handle.html;
      },
      set html(value: string) {
        handle.html = value;
      },
      onDidReceiveMessage(handler: (message: unknown) => void): StubDisposable {
        inbound.push(handler);
        return {
          dispose: () => {
            const at = inbound.indexOf(handler);
            if (at >= 0) inbound.splice(at, 1);
          },
        };
      },
      postMessage(_message: unknown): Promise<boolean> {
        // Nothing in the extension reads a reply to this, and modelling one
        // would invent a channel the panel does not use.
        return Promise.resolve(true);
      },
    },
    reveal(_column?: number): void {
      handle.revealCount += 1;
    },
    onDidDispose(
      handler: () => void,
      _thisArg?: unknown,
      disposables?: StubDisposable[]
    ): StubDisposable {
      onDispose.push(handler);
      const registration: StubDisposable = { dispose: () => undefined };
      disposables?.push(registration);
      return registration;
    },
    dispose(): void {
      if (handle.disposed) return;
      handle.disposed = true;
      for (const handler of [...onDispose]) handler();
    },
  };

  return { handle, surface };
}

export class Uri {
  private constructor(
    readonly scheme: string,
    readonly fsPath: string
  ) {}

  static file(fsPath: string): Uri {
    return new Uri('file', fsPath);
  }

  static parse(value: string): Uri {
    return new Uri('file', value.startsWith('file://') ? value.slice('file://'.length) : value);
  }

  toString(): string {
    return `file://${this.fsPath}`;
  }
}

export class RelativePattern {
  constructor(
    readonly base: Uri,
    readonly pattern: string
  ) {}
}

export class Position {
  constructor(
    readonly line: number,
    readonly character: number
  ) {}
}

export class Range {
  constructor(
    readonly start: Position,
    readonly end: Position
  ) {}
}

export class Diagnostic {
  source?: string;

  constructor(
    readonly range: Range,
    readonly message: string,
    readonly severity?: number
  ) {}
}

export const DiagnosticSeverity = {
  Error: 0,
  Warning: 1,
  Information: 2,
  Hint: 3,
} as const;

export const workspace = {
  // Mutable: a case sets the trust state it wants before calling activate().
  isTrusted: true,
  workspaceFolders: undefined as { uri: Uri }[] | undefined,
  textDocuments: [] as { uri: Uri; getText(): string; isDirty: boolean }[],

  getConfiguration(section: string) {
    return {
      // No type parameter, unlike the real `inspect<T>`: the generic is erased
      // before this file ever runs, and src/extension.ts type-checks against
      // the genuine @types/vscode rather than against this.
      inspect(key: string): ConfigurationValues | undefined {
        return settings.get(`${section}.${key}`);
      },
    };
  },

  createFileSystemWatcher(pattern: RelativePattern): StubDisposable & {
    onDidChange(handler: () => void): StubDisposable;
    onDidCreate(handler: () => void): StubDisposable;
    onDidDelete(handler: () => void): StubDisposable;
  } {
    recorded.watched.push(`${pattern.base.fsPath}/${pattern.pattern}`);
    const noop = (): StubDisposable => ({ dispose: () => undefined });
    return {
      onDidChange: noop,
      onDidCreate: noop,
      onDidDelete: noop,
      dispose: () => undefined,
    };
  },

  onDidGrantWorkspaceTrust(listener: () => void): StubDisposable {
    trustListener = listener;
    return {
      dispose: () => {
        trustListener = undefined;
      },
    };
  },
};

export const window = {
  showErrorMessage(message: string): Promise<undefined> {
    recorded.errors.push(message);
    return Promise.resolve(undefined);
  },

  showInformationMessage(message: string, ..._items: string[]): Promise<undefined> {
    recorded.infos.push(message);
    return Promise.resolve(undefined);
  },

  withProgress<T>(
    _options: unknown,
    task: (
      progress: { report(value: { message?: string }): void },
      token: { onCancellationRequested(listener: () => void): StubDisposable },
    ) => Promise<T>,
  ): Promise<T> {
    return task(
      { report: () => undefined },
      { onCancellationRequested: () => ({ dispose: () => undefined }) },
    );
  },

  showWarningMessage(message: string): Promise<undefined> {
    recorded.warnings.push(message);
    return Promise.resolve(undefined);
  },

  registerTreeDataProvider(viewId: string, _provider: unknown): StubDisposable {
    recorded.treeViews.push(viewId);
    return { dispose: () => undefined };
  },

  // What the "+" menu reaches for (memql#3412). Answers "the operator dismissed
  // it", which is the outcome that makes a command handler return without doing
  // anything -- the only behaviour this stub is asked for, since the menu's
  // DECISION is unit-tested away from `vscode` (src/clusters/presence.ts) and
  // its RENDERING in the host smoke lane.
  //
  // showInformationMessage is NOT redeclared here: the one above already
  // records into `recorded.infos`, which is strictly more useful than a silent
  // resolve, and env/clipboard already carries the throwing contract memql#3403
  // established and memql#3411 extended.
  showQuickPick(_items: unknown, _options?: unknown): Promise<undefined> {
    return Promise.resolve(undefined);
  },

  // The add-a-cluster page (memql#3472, driven by memql#3514). Every panel it
  // creates is kept on `recorded.webviews` so a case can read what was
  // rendered and post what a click would post.
  createWebviewPanel(
    viewType: string,
    title: string,
    _column?: number,
    _options?: unknown
  ): WebviewPanelSurface {
    const { handle, surface } = createStubWebviewPanel(viewType, title);
    recorded.webviews.push(handle);
    return surface;
  },
};

// The progress-notification location src/extension.ts names for the sign-in
// flow (memql#3403). A constant, not behaviour: activation only has to be able
// to READ it, because withProgress is reached from a command handler and this
// stub deliberately models nothing past activation.
export const ProgressLocation = {
  SourceControl: 1,
  Window: 10,
  Notification: 15,
} as const;

// The host capabilities the sign-in flows bind: asExternalUri / openExternal
// for the loopback flow (memql#3403) and clipboard.writeText for the
// device-code copy button (memql#3411). Present so the imports in
// src/extension.ts resolve -- an absent named export is a bundle error, not a
// lazy failure.
//
// All three THROW rather than resolving, and that is the point. Nothing in
// activation may open a browser or touch the clipboard, so a silent no-op here
// would let a case that wandered into a sign-in path pass while asserting
// nothing. What those handlers actually do is host-lane territory.
export const env = {
  asExternalUri(_uri: Uri): Promise<Uri> {
    throw new Error('vscodeStub: env.asExternalUri is out of scope -- this stub models activation only');
  },
  openExternal(_uri: Uri): Promise<boolean> {
    throw new Error('vscodeStub: env.openExternal is out of scope -- this stub models activation only');
  },
  clipboard: {
    writeText(_value: string): Promise<void> {
      throw new Error('vscodeStub: env.clipboard.writeText is out of scope -- this stub models activation only');
    },
  },
};

export const commands = {
  registerCommand(id: string, _handler: (...args: never[]) => unknown): StubDisposable {
    recorded.commands.push(id);
    return { dispose: () => undefined };
  },

  // RECORDS AND RESOLVES, rather than dispatching to the registered handler.
  // The panel's hand-off reaches the tree and the sign-in flow this way
  // (memql#3477), and both are surfaces of their own with their own tests --
  // what the panel is responsible for is asking for them, in order, which is
  // what the id list preserves.
  executeCommand(id: string, ..._args: unknown[]): Promise<undefined> {
    recorded.executed.push(id);
    return Promise.resolve(undefined);
  },
};

export const languages = {
  createDiagnosticCollection(name: string) {
    return {
      name,
      set: () => undefined,
      clear: () => undefined,
      dispose: () => undefined,
    };
  },

  registerCodeLensProvider(_selector: unknown, _provider: unknown): StubDisposable {
    return { dispose: () => undefined };
  },
};

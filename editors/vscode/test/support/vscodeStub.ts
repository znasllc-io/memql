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
//
// AND THE FILE DECORATION SURFACE (memql#4244). `ReadonlyMarker` decides
// nothing -- constructs/readonly.ts does -- but the decoration it BUILDS is the
// half an operator sees, and the editor drops a decoration whose badge is too
// long rather than rendering it, so "there is a badge, and it is short enough"
// is a claim worth an assertion rather than an eyeball. That needs
// `FileDecoration`, `ThemeColor`, the two workspace path helpers and a
// `files.readonlyInclude` that can be read back, so they are modelled below.

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
  /**
   * View ids passed to window.registerTreeDataProvider OR window.createTreeView.
   *
   * ONE LIST FOR BOTH, because the question every caller asks it is "did
   * activation put this view up", and the two APIs are two ways of doing that.
   * Deployments moved to `createTreeView` in memql#4426 only because it needs
   * the TreeView object back -- `TreeView.description` is where the selected
   * cluster's facts now live -- and splitting the list would have made that
   * implementation detail into a change every activation assertion had to
   * follow.
   */
  treeViews: [] as string[],
  /**
   * Descriptions written to a TreeView handed back by createTreeView, by view
   * id. The Deployments view's instance line (memql#4426) is the only writer.
   */
  treeViewDescriptions: {} as Record<string, string | undefined>,
  /** How many times window.registerFileDecorationProvider was called. */
  fileDecorationProviders: 0,
  /** How many times window.registerUriHandler was called (memql#4251). */
  uriHandlers: 0,
  /** Uri schemes passed to workspace.registerTextDocumentContentProvider. */
  contentProviderSchemes: [] as string[],
  /** Command ids passed to commands.registerCommand. */
  commands: [] as string[],
  /** File-system watcher globs, as `<base>/<pattern>`. */
  watched: [] as string[],
  /** Command ids passed to commands.executeCommand, in order. */
  executed: [] as string[],
  /** Every panel window.createWebviewPanel has produced, in order. */
  webviews: [] as StubWebviewPanel[],
  /** Every window.showOpenDialog invocation, with the options it was given. */
  openDialogs: [] as Record<string, unknown>[],
  /** Every window.showInputBox invocation, with the options it was given. */
  inputBoxes: [] as Record<string, unknown>[],
  /** Every terminal created, with what was typed into it (memql#3551). */
  terminals: [] as { name: string; shown: boolean; sent: { text: string; executed: boolean }[] }[],
  /** Every output channel created, with what was written to it (memql#3763). */
  outputChannels: [] as { name: string; lines: string[]; shown: boolean }[],
};

/**
 * What the next `window.showOpenDialog` answers with (memql#3547).
 *
 * `undefined` is CANCELLED, which is the case worth being able to drive: a
 * cancelled picker must leave the form exactly as it was, and that is not
 * observable unless a test can cancel one.
 */
export let nextOpenDialogResult: Uri[] | undefined;

/** Arms the next open dialog. Pass undefined for "the operator cancelled". */
export function setNextOpenDialogResult(result: Uri[] | undefined): void {
  nextOpenDialogResult = result;
}

/**
 * What the next `window.showInputBox` answers with (memql#3568).
 *
 * DEFAULT UNDEFINED, meaning dismissed -- so a case that says nothing about the
 * password prompt gets a run that collects none, which is the behaviour a test
 * about something else should see. A test that wants the one-password path
 * arms it deliberately.
 */
export let nextInputBoxResult: string | undefined;

export function setNextInputBoxResult(result: string | undefined): void {
  nextInputBoxResult = result;
}

/** Drops everything `recorded` holds. Call between cases. */
export function resetRecorded(): void {
  recorded.errors.length = 0;
  recorded.warnings.length = 0;
  recorded.infos.length = 0;
  recorded.treeViews.length = 0;
  for (const key of Object.keys(recorded.treeViewDescriptions)) {
    delete recorded.treeViewDescriptions[key];
  }
  recorded.fileDecorationProviders = 0;
  recorded.commands.length = 0;
  recorded.watched.length = 0;
  recorded.executed.length = 0;
  recorded.webviews.length = 0;
  recorded.openDialogs.length = 0;
  recorded.terminals.length = 0;
  // The password prompt, which this reset MISSED until memql#3586 -- the
  // recording landed with the one-password agent (memql#3568) and nothing read it
  // until a case asserted that a NOPASSWD machine is never asked, then counted
  // two prompts belonging to the two cases before it. A per-case recorder that
  // silently accumulates across cases is worse than none: it reports a fact
  // about the whole file while reading like a fact about one case.
  recorded.inputBoxes.length = 0;
  recorded.outputChannels.length = 0;
  // Appearance listeners and the editor's theme kind (memql#4419). Same
  // reasoning as the input boxes above, one step worse: a listener left
  // registered by a previous case belongs to a DISPOSED panel, so the next
  // fireConfigurationChange() renders into a closed tab and the render counts
  // this lane asserts on come out one high, blamed on the wrong panel.
  resetAppearanceListeners();
  window.activeColorTheme = { kind: ColorThemeKind.Dark };
  nextOpenDialogResult = undefined;
  // Back to DISMISSED, the documented default. An armed answer surviving into the
  // next case would hand a password to a run that never asked for one.
  nextInputBoxResult = undefined;
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

// -----------------------------------------------------------------------------
// The appearance surface (memql#4419)
// -----------------------------------------------------------------------------
//
// Three members and two drivers, added because the panels now RE-RENDER on
// them. What `memql.appearance` resolves to is unit-tested away from `vscode`
// in src/webview/appearance.ts; what is only observable here is the half that
// makes the setting worth having -- that flipping it repaints an OPEN panel,
// rather than taking effect the next time one is opened.

export const ColorThemeKind = {
  Light: 1,
  Dark: 2,
  HighContrast: 3,
  HighContrastLight: 4,
} as const;

/** Listeners registered through `window.onDidChangeActiveColorTheme`. */
const colorThemeListeners: ((theme: { kind: number }) => void)[] = [];
/** Listeners registered through `workspace.onDidChangeConfiguration`. */
const configurationListeners: ((event: { affectsConfiguration(section: string): boolean }) => void)[] = [];

/**
 * Switch the editor's theme kind and fire the listeners, as the editor does.
 *
 * DEFAULTS TO DARK at module load, because that is the case where forcing
 * `light` is visible: under a light editor a forced-light panel is
 * indistinguishable from an unstamped one, so a test that started light could
 * pass against a panel that stamps nothing at all.
 */
export function setActiveColorTheme(kind: number): void {
  window.activeColorTheme = { kind };
  for (const listener of [...colorThemeListeners]) listener(window.activeColorTheme);
}

/**
 * Fire a configuration-change event naming the sections that changed.
 *
 * `affectsConfiguration` answers by PREFIX, the way the real one does, so a
 * listener asking about `memql.appearance` is woken by a change reported as
 * `memql` -- and, importantly, is NOT woken by `memql.lsp.serverPath`. A stub
 * that answered true for everything would let a listener with a missing or
 * wrong guard pass this lane and repaint every panel on every keystroke in
 * settings.json.
 */
export function fireConfigurationChange(...changed: string[]): void {
  const event = {
    affectsConfiguration(section: string): boolean {
      return changed.some((c) => section === c || section.startsWith(`${c}.`) || c.startsWith(`${section}.`));
    },
  };
  for (const listener of [...configurationListeners]) listener(event);
}

/** Drop every registered appearance listener. Called by resetRecorded(). */
function resetAppearanceListeners(): void {
  colorThemeListeners.length = 0;
  configurationListeners.length = 0;
}

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
  /**
   * How many times the extension has ASSIGNED that html.
   *
   * Not a render statistic. Assigning `webview.html` replaces the whole
   * document, which destroys the DOM -- and with it the focused element, the
   * caret and any selection. So this count is the only host-side evidence of a
   * property the operator experiences directly: whether the page they are
   * typing into is still the page they started typing into (memql#3538).
   */
  renders: number;
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
    renders: 0,
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
        handle.renders += 1;
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

/**
 * The decoration shape, WITHOUT the editor's validation.
 *
 * Deliberately: the real class throws on a badge longer than two code points
 * and the extension host then drops the decoration and logs a warning, which is
 * a failure a test must be able to SEE rather than inherit. So this records
 * what was built and readonly.test.ts asserts the length rule against
 * `reasonBadge` directly.
 */
export class FileDecoration {
  propagate?: boolean;

  constructor(
    readonly badge?: string,
    public tooltip?: string,
    readonly color?: ThemeColor
  ) {}
}

export class ThemeColor {
  constructor(readonly id: string) {}
}

// ---------------------------------------------------------------------------
// the tree-item surface (memql#4425, memql#4427)
// ---------------------------------------------------------------------------
//
// A DELIBERATE WIDENING of what this stub claims to model, and the header's
// rule -- "a member reached only from a tree item render is absent on purpose"
// -- is what makes it worth writing down rather than just adding.
//
// Two acceptance items cannot be checked without it. The first is that the
// three cluster-backed providers return `[]` when no cluster is selected:
// VS Code renders `viewsWelcome` content ONLY over a genuinely empty tree, so a
// single stray row silently deletes the welcome, and that failure is invisible
// to every other lane -- the extension activates, the view has content, and the
// content is wrong. The second is that a deployment row carries
// `memql.deployments.openRun`: a row with no command is a click that does
// nothing, which is the defect memql#4427 exists to fix, and it is a property
// of the TreeItem this fake now has to be able to hold.
//
// It models the FIELDS a `getTreeItem` sets and no behaviour: no icon
// resolution, no markdown rendering, no collapse state machine. A case that
// starts depending on any of those should fail here rather than pass against a
// fiction, exactly as the header argues.

export const TreeItemCollapsibleState = {
  None: 0,
  Collapsed: 1,
  Expanded: 2,
} as const;

export class ThemeIcon {
  constructor(
    readonly id: string,
    readonly color?: ThemeColor
  ) {}
}

export class MarkdownString {
  supportHtml = false;

  constructor(public value = "") {}
}

export interface StubCommand {
  command: string;
  title: string;
  arguments?: unknown[];
}

export class TreeItem {
  description?: string;
  tooltip?: string | MarkdownString;
  iconPath?: ThemeIcon;
  contextValue?: string;
  command?: StubCommand;

  constructor(
    public label: string,
    public collapsibleState: number = TreeItemCollapsibleState.None
  ) {}
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

// A CodeLens is constructed only inside a provider callback, which activation
// never invokes -- but the import is resolved when esbuild BUNDLES the
// extension, so an absent export fails the whole build rather than the one case
// that would have reached it. Present for that reason and modelled no further.
export class CodeLens {
  constructor(
    readonly range: Range,
    readonly command?: { title: string; command: string; tooltip?: string; arguments?: unknown[] }
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

/** What `getConfiguration(section).update(key, value)` wrote, by `section.key`. */
const written = new Map<string, unknown>();

/**
 * Every `update` call, in order -- INCLUDING the ones that remove a key.
 *
 * Kept separately from the values because "wrote nothing" and "wrote the same
 * thing again" are different facts about somebody's `.vscode/settings.json`,
 * and a store that only remembers the last value cannot tell them apart. The
 * second is a modified tracked file (memql#4244).
 */
const writes: { sectionKey: string; value: unknown }[] = [];

/** Read a written setting back, count the writes, or clear both between cases. */
export const writtenSettings = {
  get(sectionKey: string): unknown {
    return written.get(sectionKey);
  },
  set(sectionKey: string, value: unknown): void {
    written.set(sectionKey, value);
  },
  /** Every update call so far, oldest first. A removal has `value: undefined`. */
  writes(): readonly { sectionKey: string; value: unknown }[] {
    return writes;
  },
  clear(): void {
    written.clear();
    writes.length = 0;
  },
};

export const ConfigurationTarget = {
  Global: 1,
  Workspace: 2,
  WorkspaceFolder: 3,
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
      // Read/write, for the one caller that owns a settings key rather than
      // reading one: ReadonlyMarker rewrites `files.readonlyInclude`. Stored
      // whole so a case can assert what was written AND that a key the fake
      // operator added by hand survived.
      get(key: string): unknown {
        return written.get(`${section}.${key}`);
      },
      // `undefined` REMOVES the setting, as the real one does -- so a case can
      // tell "we deleted our key" from "we never wrote".
      update(key: string, value: unknown): Promise<void> {
        const sectionKey = `${section}.${key}`;
        writes.push({ sectionKey, value });
        if (value === undefined) written.delete(sectionKey);
        else written.set(sectionKey, value);
        return Promise.resolve();
      },
    };
  },

  /**
   * The folder a file belongs to, matched by path prefix.
   *
   * Prefix rather than exact, because that is the question the real one
   * answers, and undefined for a file outside every folder -- which is a case
   * ReadonlyMarker treats as unclassifiable rather than as editable.
   */
  getWorkspaceFolder(uri: Uri): { uri: Uri } | undefined {
    return (workspace.workspaceFolders ?? []).find(
      (f) => uri.fsPath === f.uri.fsPath || uri.fsPath.startsWith(`${f.uri.fsPath}/`)
    );
  },

  asRelativePath(uri: Uri, _includeWorkspaceFolder?: boolean): string {
    const folder = workspace.getWorkspaceFolder(uri);
    if (folder === undefined) return uri.fsPath;
    return uri.fsPath.slice(folder.uri.fsPath.length).replace(/^\//, '');
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

  // Registration only, and the SCHEME is what is recorded: nothing in this lane
  // opens a document, so the provider is never asked for content -- what a unit
  // test can see about it is that activation claimed the scheme at all.
  registerTextDocumentContentProvider(scheme: string, _provider: unknown): StubDisposable {
    recorded.contentProviderSchemes.push(scheme);
    return { dispose: () => undefined };
  },

  onDidGrantWorkspaceTrust(listener: () => void): StubDisposable {
    trustListener = listener;
    return {
      dispose: () => {
        trustListener = undefined;
      },
    };
  },

  // What every panel host subscribes to for `memql.appearance` (memql#4419).
  // Driven by fireConfigurationChange().
  onDidChangeConfiguration(
    listener: (event: { affectsConfiguration(section: string): boolean }) => void
  ): StubDisposable {
    configurationListeners.push(listener);
    return {
      dispose: () => {
        const at = configurationListeners.indexOf(listener);
        if (at >= 0) configurationListeners.splice(at, 1);
      },
    };
  },
};

export const window = {
  /**
   * The editor's current theme, as `vscode.window.activeColorTheme`.
   *
   * MUTABLE and replaced wholesale by setActiveColorTheme(), rather than
   * having its `kind` poked, because the real one is a fresh object per change
   * -- a panel that cached the object rather than reading it per render would
   * pass against an in-place mutation and fail in the editor.
   */
  activeColorTheme: { kind: ColorThemeKind.Dark as number },

  /** Panel hosts subscribe here to repaint when the editor's theme changes. */
  onDidChangeActiveColorTheme(listener: (theme: { kind: number }) => void): StubDisposable {
    colorThemeListeners.push(listener);
    return {
      dispose: () => {
        const at = colorThemeListeners.indexOf(listener);
        if (at >= 0) colorThemeListeners.splice(at, 1);
      },
    };
  },

  showErrorMessage(message: string): Promise<undefined> {
    recorded.errors.push(message);
    return Promise.resolve(undefined);
  },

  // The training surface's record channel (memql#3763). Created during
  // ACTIVATION rather than on first use, so it sits on the path a trusted
  // activation takes and has to exist here even though nothing in this lane
  // writes to one.
  createOutputChannel(name: string): {
    appendLine(line: string): void;
    show(preserveFocus?: boolean): void;
    dispose(): void;
  } {
    const entry = { name, lines: [] as string[], shown: false };
    recorded.outputChannels.push(entry);
    return {
      appendLine(line: string): void {
        entry.lines.push(line);
      },
      show(): void {
        entry.shown = true;
      },
      dispose(): void {},
    };
  },

  showInformationMessage(message: string, ..._items: string[]): Promise<undefined> {
    recorded.infos.push(message);
    return Promise.resolve(undefined);
  },

  // The privileged-command handoff (memql#3551). The extension spawns every
  // capability unprivileged, so a step needing root is run by the OPERATOR in
  // their own terminal -- which makes "was it typed or was it executed" a
  // property worth being able to assert.
  createTerminal(options: { name?: string } | string): {
    show(): void;
    sendText(text: string, addNewLine?: boolean): void;
    dispose(): void;
  } {
    const entry = {
      name: typeof options === "string" ? options : (options.name ?? ""),
      shown: false,
      sent: [] as { text: string; executed: boolean }[],
    };
    recorded.terminals.push(entry);
    return {
      show(): void {
        entry.shown = true;
      },
      sendText(text: string, addNewLine?: boolean): void {
        // VS Code's default is TRUE -- omitting the flag RUNS the command.
        entry.sent.push({ text, executed: addNewLine !== false });
      },
      dispose(): void {},
    };
  },

  // The file picker the key-file field offers (memql#3547). A webview cannot
  // open one itself, so this is the host-side half the page posts to reach.
  showOpenDialog(options: Record<string, unknown>): Promise<Uri[] | undefined> {
    recorded.openDialogs.push(options);
    return Promise.resolve(nextOpenDialogResult);
  },

  showInputBox(options: Record<string, unknown>): Promise<string | undefined> {
    recorded.inputBoxes.push(options);
    return Promise.resolve(nextInputBoxResult);
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

  /**
   * The TreeView object, modelled only as far as this extension uses it
   * (memql#4426): a disposable that carries a settable `description`.
   *
   * Not a full TreeView -- there is no reveal, no selection, no visibility
   * event -- and that is the stub's standing rule rather than an omission: it
   * models what activation touches, so a test that starts depending on more
   * fails loudly here instead of passing against a fiction.
   */
  createTreeView(viewId: string, _options: unknown): StubDisposable & { description?: string } {
    recorded.treeViews.push(viewId);
    const view = {
      dispose: () => undefined,
      get description(): string | undefined {
        return recorded.treeViewDescriptions[viewId];
      },
      set description(value: string | undefined) {
        recorded.treeViewDescriptions[viewId] = value;
      },
    };
    return view;
  },

  // The read-only badge (memql#3762). Recorded rather than ignored, because
  // "activation registers it" is the only thing about it activation can assert
  // -- what it DECIDES is unit-tested away from `vscode` in
  // src/constructs/readonly.ts, which is the point of the split.
  registerFileDecorationProvider(_provider: unknown): StubDisposable {
    recorded.fileDecorationProviders += 1;
    return { dispose: () => undefined };
  },

  // The portal handoff's entry point (memql#4251). Counted rather than driven:
  // WHEN it is registered is an activation fact and belongs here -- it has to
  // happen outside the trust gate, or the link that woke the editor arrives
  // before any handler exists. What the handler DECIDES is unit-tested away
  // from `vscode` in src/handoff/, and driven end to end in the host lane,
  // which is the only place a real `vscode.Uri` and a real globalState exist.
  registerUriHandler(_handler: unknown): StubDisposable {
    recorded.uriHandlers += 1;
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

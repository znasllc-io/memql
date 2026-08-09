// A hand-written stand-in for the editor-injected `vscode` module, used by the
// activation cases (memql#3387) and by nothing else.
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
// WHAT IT COVERS. The activation path and nothing more -- the members
// activate() and registerRuntimeSurface() actually touch. A member reached only
// from a command handler, a tree item render or a webview is absent on purpose:
// an absent member fails loudly the moment a case wanders past what this stub
// claims to model, which is the right outcome for a fake.

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
  /** View ids passed to window.registerTreeDataProvider. */
  treeViews: [] as string[],
  /** Command ids passed to commands.registerCommand. */
  commands: [] as string[],
  /** File-system watcher globs, as `<base>/<pattern>`. */
  watched: [] as string[],
};

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

  showWarningMessage(message: string): Promise<undefined> {
    recorded.warnings.push(message);
    return Promise.resolve(undefined);
  },

  registerTreeDataProvider(viewId: string, _provider: unknown): StubDisposable {
    recorded.treeViews.push(viewId);
    return { dispose: () => undefined };
  },
};

export const commands = {
  registerCommand(id: string, _handler: (...args: never[]) => unknown): StubDisposable {
    recorded.commands.push(id);
    return { dispose: () => undefined };
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

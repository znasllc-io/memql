export class Uri {
  constructor(private readonly value: URL) {}
  static parse(value: string): Uri { return new Uri(new URL(value)); }
  static file(path: string): Uri { return Uri.from({ scheme: "file", path }); }
  static from(parts: { scheme: string; authority?: string; path: string }): Uri {
    const url = new URL(`${parts.scheme}://${parts.authority || ""}/`); url.pathname = parts.path; return new Uri(url);
  }
  static joinPath(base: Uri, ...parts: string[]): Uri { return base.with({ path: [base.path.replace(/\/$/, ""), ...parts].join("/") }); }
  get scheme(): string { return this.value.protocol.slice(0, -1); }
  get authority(): string { return this.value.host; }
  get path(): string { return this.value.pathname; }
  get fsPath(): string { return decodeURIComponent(this.value.pathname); }
  with(change: { path: string }): Uri { const url = new URL(this.value); url.pathname = change.path; return new Uri(url); }
  toString(): string { return this.value.toString(); }
}
export class EventEmitter<T> { event = () => ({ dispose() {} }); fire(_event: T): void {} dispose(): void {} }
export class Position { constructor(readonly line: number, readonly character: number) {} }
export class Range { constructor(readonly start: Position, readonly end: Position) {} }
/** A workspace edit as the tests read it: the replacements it carries, in order. */
export class WorkspaceEdit {
  readonly replacements: { uri: Uri; range: Range; text: string }[] = [];
  replace(uri: Uri, range: Range, text: string): void { this.replacements.push({ uri, range, text }); }
}
export class TabInputCustom { constructor(readonly uri: Uri, readonly viewType: string) {} }
export class TabInputText { constructor(readonly uri: Uri) {} }
export const stored = new Map<string, Uint8Array>();
export const writes: string[] = [];
export const window = {
  showQuickPick: async (_items: unknown[], _options?: unknown): Promise<any> => undefined,
  showInputBox: async (_options?: unknown): Promise<string | undefined> => undefined,
  showInformationMessage: async (_message: string, ..._buttons: string[]): Promise<string | undefined> => undefined,
  showErrorMessage: async (_message: string, ..._buttons: string[]): Promise<string | undefined> => undefined,
  showOpenDialog: async (_options?: unknown): Promise<Uri[] | undefined> => undefined,
  showTextDocument: async (_document: unknown, _options?: unknown): Promise<unknown> => undefined,
  onDidChangeVisibleTextEditors: (_listener: unknown) => ({ dispose() {} }),
  visibleTextEditors: [] as { document: unknown; viewColumn?: number }[],
  activeTextEditor: undefined as { document: { uri: Uri } } | undefined,
  tabGroups: { all: [] as { tabs: { input: unknown }[] }[], close: async (_tabs: unknown): Promise<boolean> => true },
};
export const commands = { registerCommand: (_name:string,_handler:(...args:any[])=>any)=>({dispose(){}}), getCommands:async()=>[] as string[], executeCommand: async (_name: string, ..._args: unknown[]): Promise<unknown> => undefined };
export const workspace = {
  fs: {
    async readFile(uri: Uri) { const bytes = stored.get(uri.toString()); if (!bytes) throw new Error("not found"); return new Uint8Array(bytes); },
    async writeFile(uri: Uri, bytes: Uint8Array) { writes.push(uri.toString()); stored.set(uri.toString(), new Uint8Array(bytes)); },
    async delete(uri: Uri) { stored.delete(uri.toString()); },
    async stat(uri: Uri) { const bytes = stored.get(uri.toString()); if (!bytes) throw new Error("not found"); return { type: 1, ctime: 0, mtime: 0, size: bytes.length }; },
  },
  applyEdit: async (_edit: WorkspaceEdit): Promise<boolean> => true,
  openTextDocument: async (_uri: Uri): Promise<unknown> => undefined,
  onDidChangeTextDocument: (_listener: unknown) => ({ dispose() {} }),
  onDidSaveTextDocument: (_listener: unknown) => ({ dispose() {} }),
};

export const ProgressLocation = { Notification: 15 };
export const ConfigurationTarget = {Global:1};
export const ColorThemeKind = {Light:1,Dark:2,HighContrast:3,HighContrastLight:4};
export const ViewColumn = {Active:-1,Beside:-2,One:1,Two:2,Three:3};
export const EndOfLine = { LF: 1, CRLF: 2 };
export const env = { clipboard: { writeText: async (_text: string): Promise<void> => {} }, openExternal: async (_target: Uri): Promise<boolean> => true };

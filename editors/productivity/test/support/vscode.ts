export class Uri {
  constructor(private readonly value: URL) {}
  static parse(value: string): Uri { return new Uri(new URL(value)); }
  static from(parts: { scheme: string; authority?: string; path: string }): Uri {
    const url = new URL(`${parts.scheme}://${parts.authority || ""}/`); url.pathname = parts.path; return new Uri(url);
  }
  get scheme(): string { return this.value.protocol.slice(0, -1); }
  get path(): string { return this.value.pathname; }
  with(change: { path: string }): Uri { const url = new URL(this.value); url.pathname = change.path; return new Uri(url); }
  toString(): string { return this.value.toString(); }
}
export class EventEmitter<T> { event = () => ({ dispose() {} }); fire(_event: T): void {} dispose(): void {} }
export const stored = new Map<string, Uint8Array>();
export const writes: string[] = [];
export const window = {
  showQuickPick: async (_items: unknown[], _options?: unknown): Promise<any> => undefined,
  showInputBox: async (_options?: unknown): Promise<string | undefined> => undefined,
  showInformationMessage: async (_message: string): Promise<void> => {},
};
export const commands = { executeCommand: async (_name: string, ..._args: unknown[]): Promise<void> => {} };
export const workspace = { fs: {
  async readFile(uri: Uri) { const bytes = stored.get(uri.toString()); if (!bytes) throw new Error("not found"); return new Uint8Array(bytes); },
  async writeFile(uri: Uri, bytes: Uint8Array) { writes.push(uri.toString()); stored.set(uri.toString(), new Uint8Array(bytes)); },
  async delete(uri: Uri) { stored.delete(uri.toString()); },
} };

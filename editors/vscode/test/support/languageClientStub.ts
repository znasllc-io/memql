// A stand-in for `vscode-languageclient/node`, the companion to
// test/support/vscodeStub.ts (memql#3387).
//
// WHY IT IS NEEDED. The real package subclasses editor-supplied classes at
// MODULE LOAD -- `class ProtocolCompletionItem extends CompletionItem` and a
// dozen like it -- so merely requiring it against a fake `vscode` throws
// "Class extends value undefined". Growing the vscode stub until an
// 8,000-line client package loads would mean modelling most of vscode.d.ts to
// test an ordering decision in activate(), which is the wrong trade.
//
// WHAT IT ASSERTS BY EXISTING. The activation cases arrange for NO memql-lsp
// binary, so a correct activate() never reaches `new LanguageClient(...)` at
// all. `constructed` records the calls, and a case that expects the language
// server to stay down can say so directly.

export const TransportKind = {
  stdio: 0,
  ipc: 1,
  pipe: 2,
  socket: 3,
} as const;

/** Ids passed to `new LanguageClient(id, ...)`, in construction order. */
export const constructed: string[] = [];

export class LanguageClient {
  initializeResult: undefined;

  constructor(
    readonly id: string,
    readonly name: string,
    readonly serverOptions: unknown,
    readonly clientOptions: unknown
  ) {
    constructed.push(id);
  }

  start(): Promise<void> {
    return Promise.resolve();
  }

  stop(): Promise<void> {
    return Promise.resolve();
  }

  sendRequest(_method: string, _params: unknown): Promise<unknown> {
    throw new Error('languageClientStub: sendRequest is not modelled');
  }
}

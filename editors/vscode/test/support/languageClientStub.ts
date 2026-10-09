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
// Records whether activation starts a client and how it was configured.
// Missing-binary cases expect none; deferred-start cases supply a binary and
// expect a client only after opening a local MemQL document.

export const TransportKind = {
  stdio: 0,
  ipc: 1,
  pipe: 2,
  socket: 3,
} as const;

/** Ids passed to `new LanguageClient(id, ...)`, in construction order. */
export const constructed: string[] = [];
export const instances: LanguageClient[] = [];

export class LanguageClient {
  initializeResult: undefined;

  constructor(
    readonly id: string,
    readonly name: string,
    readonly serverOptions: unknown,
    readonly clientOptions: unknown
  ) {
    constructed.push(id);
    instances.push(this);
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

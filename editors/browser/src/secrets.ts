// Browser VS Code owns its credentials through SecretStorage. This adapter
// persists opaque values encrypted with a non-extractable, origin-bound key.
// No token enters a URL, OS storage, webview origin or second cluster registry.
export class BrowserSecrets {
  readonly type = 'persisted' as const;
  private readonly database: Promise<IDBDatabase>;
  constructor() {
    this.database = new Promise((resolve, reject) => {
      const request = indexedDB.open('memql-editor-secrets', 1);
      request.onupgradeneeded = () => {
        request.result.createObjectStore('key');
        request.result.createObjectStore('secrets');
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(new Error('Browser secret storage is unavailable. Allow storage for this editor, then try again.'));
      request.onblocked = () => reject(new Error('Close other editor tabs and try again to unlock browser secret storage.'));
    });
  }
  private async request<T>(store: string, mode: IDBTransactionMode, run: (store: IDBObjectStore) => IDBRequest<T>): Promise<T> {
    const db = await this.database;
    return new Promise((resolve, reject) => {
      const transaction = db.transaction(store, mode);
      const request = run(transaction.objectStore(store));
      transaction.oncomplete = () => resolve(request.result);
      transaction.onerror = transaction.onabort = () => reject(transaction.error ?? new Error('Browser secret storage could not complete the operation.'));
    });
  }
  private async key(): Promise<CryptoKey> {
    const existing = await this.request<CryptoKey | undefined>('key', 'readonly', store => store.get('encryption'));
    if (existing) return existing;
    const generated = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
    const db = await this.database;
    return new Promise((resolve, reject) => {
      // Concurrent tabs reuse the first committed key, never replace it.
      const transaction = db.transaction('key', 'readwrite');
      const store = transaction.objectStore('key');
      const request = store.get('encryption');
      let selected = generated;
      request.onsuccess = () => {
        if (request.result) selected = request.result;
        else store.put(generated, 'encryption');
      };
      transaction.oncomplete = () => resolve(selected);
      transaction.onerror = transaction.onabort = () => reject(transaction.error ?? new Error('Browser secret key could not be stored.'));
    });
  }
  async get(name: string): Promise<string | undefined> {
    const record = await this.request<{ iv: Uint8Array<ArrayBuffer>; bytes: ArrayBuffer } | undefined>('secrets', 'readonly', store => store.get(name));
    if (!record) return undefined;
    const bytes = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: record.iv, additionalData: new TextEncoder().encode(name) }, await this.key(), record.bytes);
    return new TextDecoder().decode(bytes);
  }
  async set(name: string, value: string): Promise<void> {
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const bytes = await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: new TextEncoder().encode(name) }, await this.key(), new TextEncoder().encode(value));
    await this.request('secrets', 'readwrite', store => store.put({ iv, bytes }, name));
  }
  async delete(name: string): Promise<void> { await this.request('secrets', 'readwrite', store => store.delete(name)); }
  async keys(): Promise<string[]> { return (await this.request('secrets', 'readonly', store => store.getAllKeys())).map(String); }
}

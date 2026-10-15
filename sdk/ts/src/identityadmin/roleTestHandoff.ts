// A one-use, exact-window/origin handoff. URLs carry only a random correlation
// nonce. Bearers remain in memory; a reload must never fall back to an owner.
export interface RoleTestCredential {
  token: string;
  sessionId: string;
  role: string;
  expiresAt: number;
  domain: string;
}
const READY = "memql.role-test.ready";
const CREDENTIAL = "memql.role-test.credential";
export const roleTestNonce = () => new URL(location.href).searchParams.get("roleTest");

const testWindows = new WeakSet<object>();
/** Once identified, a test document cannot become a normal login by changing its URL. */
export function isRoleTestWindow(): boolean {
  if (typeof window === "undefined") return false;
  if (roleTestNonce()) testWindows.add(window);
  return testWindows.has(window);
}

export function deliverRoleTest(tab: Window, destination: string, credential: RoleTestCredential): () => void {
  const url = new URL(destination);
  if (url.protocol !== "https:") throw new Error("Role tests require the browser editor over HTTPS.");
  const nonce = crypto.randomUUID();
  url.searchParams.set("roleTest", nonce);
  const stop = () => { window.removeEventListener("message", receive); clearTimeout(timer); };
  const receive = (event: MessageEvent) => {
    if (event.source !== tab || event.origin !== url.origin || event.data?.type !== READY || event.data?.nonce !== nonce) return;
    tab.postMessage({ type: CREDENTIAL, nonce, credential }, url.origin);
    stop();
  };
  const timer = setTimeout(stop, 60_000);
  window.addEventListener("message", receive);
  tab.location.replace(url.href);
  return stop;
}

export function receiveRoleTest(parentOrigin: string): Promise<RoleTestCredential> {
  const nonce = roleTestNonce();
  const parent: Window | null = window.opener;
  if (!nonce || !parent) return Promise.reject(new Error("This role-test session has ended. Start another test from Users."));
  return new Promise((resolve, reject) => {
    const stop = () => { clearTimeout(timer); window.removeEventListener("message", receive); window.opener = null; };
    const receive = (event: MessageEvent) => {
      if (event.source !== parent || event.origin !== parentOrigin || event.data?.type !== CREDENTIAL || event.data?.nonce !== nonce) return;
      const value = event.data.credential as RoleTestCredential;
      stop();
      if (!value || !/^mql_test_[0-9a-f]{64}$/.test(value.token) || !value.sessionId ||
        !["reader", "writer", "developer", "admin"].includes(value.role) ||
        !Number.isFinite(value.expiresAt) || value.expiresAt * 1000 <= Date.now() ||
        `https://os.${value.domain}` !== parentOrigin) {
        reject(new Error("The role-test handoff is invalid or expired.")); return;
      }
      resolve(value);
    };
    const timer = setTimeout(() => { stop(); reject(new Error("The role-test handoff expired. Start another test from Users.")); }, 30_000);
    window.addEventListener("message", receive);
    parent.postMessage({ type: READY, nonce }, parentOrigin);
  });
}

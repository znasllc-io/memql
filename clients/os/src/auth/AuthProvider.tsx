import { isRoleTestTab, loadRoleTest, roleTestSource, endRoleTestTab, currentRoleTest } from "./roleTest";
import { anonymousSource, identitySource, type OsAuthSource } from "./source";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import {
  isRuntimeConfigReady,
  loadRuntimeConfig,
  UNKNOWN_RUNTIME_CONFIG,
  type OsRuntimeConfig,
} from "../cluster/config";
import { authorizeUrl, exchangeCode, logout, probeSession, redirectUriFor } from "./identityClient";
import { forgetPending, rememberPending, takePending } from "./pending";
import { challengeFor, generateCodeVerifier, generateState } from "./pkce";
import { identityEntry, identityLocation, ownershipState } from "./nativeIdentity";

export type AuthStatus = "loading" | "signed-out" | "signed-in" | "unavailable" | "unclaimed";

export interface AuthContextValue {
  status: AuthStatus;
  config: OsRuntimeConfig;
  /** The credential seam (spec D7): bearer/refresh, never the raw string. */
  authSource: OsAuthSource;
  signIn: () => Promise<void>;
  signOut: () => Promise<void>;
  entry: string | null;
  completeSignIn: (destination: string) => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [config, setConfig] = useState<OsRuntimeConfig>(UNKNOWN_RUNTIME_CONFIG);
  const [status, setStatus] = useState<AuthStatus>("loading");
  const [entry, setEntry] = useState(identityEntry);
  const [sessionVersion, setSessionVersion] = useState(0);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const loaded = await loadRuntimeConfig();
        if (cancelled) return;
        setConfig(loaded);
        if (isRoleTestTab()) {
          const test = await loadRoleTest();
          if (test.domain !== loaded.domain) throw new Error("Role-test cluster mismatch");
          if (!cancelled) setStatus("signed-in");
          return;
        }
        if (!loaded.authEnabled) {
          setStatus("signed-in");
          return;
        }
        if (!isRuntimeConfigReady(loaded)) {
          setStatus("unavailable");
          return;
        }
        const pending = window.location.pathname === "/auth/callback" ? takePending() : null;
        const params = new URLSearchParams(window.location.search);
        const code = params.get("code");
        const returnedState = params.get("state");
        if (
          pending &&
          code &&
          returnedState === pending.state &&
          window.location.pathname === "/auth/callback"
        ) {
          const ok = await exchangeCode(loaded, {
            code,
            codeVerifier: pending.verifier,
            redirectUri: redirectUriFor(window.location.origin),
          });
          history.replaceState({}, "", "/");
          if (ok) {
            setStatus("signed-in");
            return;
          }
        }
        const ownership = await ownershipState(loaded);
        if (cancelled) return;
        if (ownership === "unclaimed") { setStatus("unclaimed"); return; }
        const probe = await probeSession(loaded);
        setStatus(probe.signedIn ? "signed-in" : "signed-out");
      } catch {
        if (!cancelled) setStatus("unavailable");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const signIn = useCallback(async () => {
    if (isRoleTestTab()) throw new Error("Start another role test from Users.");
    if (!isRuntimeConfigReady(config)) throw new Error("Identity configuration is unavailable");
    const verifier = await generateCodeVerifier();
    const challenge = await challengeFor(verifier);
    const state = generateState();
    if (!rememberPending(verifier, state)) throw new Error("Browser sign-in storage is unavailable");
    const destination = identityLocation(authorizeUrl(config, {
      redirectUri: redirectUriFor(window.location.origin),
      state,
      codeChallenge: challenge,
    }));
    history.replaceState({}, "", destination);
    setEntry(identityEntry());
  }, [config]);

  const completeSignIn = useCallback(async (destination: string) => {
    if (isRoleTestTab()) throw new Error("Start another role test from Users.");
    const target = new URL(destination, window.location.origin);
    if (target.origin !== window.location.origin || target.pathname !== "/auth/callback" || target.username || target.password) {
      window.location.assign(destination);
      return;
    }
    // The in-document handoff has the same one-use PKCE/state checks as a
    // callback opened by email or an external provider in a new document.
    const pending = takePending();
    const code = target.searchParams.get("code");
    if (!pending || !code || target.searchParams.get("state") !== pending.state) {
      history.replaceState({}, "", "/");
      setEntry(null);
      setStatus("unavailable");
      throw new Error("Sign-in confirmation expired. Start sign-in again.");
    }
    try {
      if (!await exchangeCode(config, { code, codeVerifier: pending.verifier, redirectUri: redirectUriFor(window.location.origin) })) {
        throw new Error("Sign-in could not be completed");
      }
    } catch (error) {
      history.replaceState({}, "", "/");
      setEntry(null);
      setStatus("unavailable");
      throw error;
    }
    history.replaceState({}, "", "/");
    setEntry(null);
    setSessionVersion(version => version + 1);
    setStatus("signed-in");
  }, [config]);

  const signOut = useCallback(async () => {
    if (isRoleTestTab()) {
      await endRoleTestTab();
      setStatus("signed-out");
      return;
    }
    forgetPending();
    // Unmount authenticated consumers before revoking their cookie, so no
    // background read competes with logout or paints a half-cleared desktop.
    setStatus("loading");
    await logout(config);
    history.replaceState({}, "", "/");
    setEntry(null);
    setStatus("signed-out");
  }, [config]);

  const authSource = useMemo<OsAuthSource>(
    // A same-document sign-in must never reuse the preceding session's bearer.
    () => (status === "signed-in" ? (isRoleTestTab() ? roleTestSource : identitySource(config)) : anonymousSource),
    [config, status, sessionVersion],
  );

  useEffect(() => {
    const test = currentRoleTest();
    if (!isRoleTestTab() || status !== "signed-in" || !test) return;
    const ended = () => setStatus("signed-out");
    window.addEventListener("memql-role-test-ended", ended);
    const timer = setTimeout(ended, Math.max(0, test.expiresAt * 1000 - Date.now()));
    return () => { clearTimeout(timer); window.removeEventListener("memql-role-test-ended", ended); };
  }, [status]);

  const value = useMemo<AuthContextValue>(
    () => ({ status, config, authSource, signIn, signOut, entry, completeSignIn }),
    [status, config, authSource, signIn, signOut, entry, completeSignIn],
  );

  return <AuthContext.Provider value={value}>{isRoleTestTab() && status !== "signed-in" && status !== "loading"
    ? <main className="os-settings"><h1>Role test ended</h1><p>Start another test from Users in your original tab.</p><button type="button" onClick={() => window.close()}>Close test tab</button></main>
    : children}</AuthContext.Provider>;
}

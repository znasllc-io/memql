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

export type AuthStatus = "loading" | "signed-out" | "signed-in" | "unavailable";

export interface AuthContextValue {
  status: AuthStatus;
  config: OsRuntimeConfig;
  signIn: () => Promise<void>;
  signOut: () => Promise<void>;
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

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const loaded = await loadRuntimeConfig();
        if (cancelled) return;
        setConfig(loaded);
        if (!loaded.authEnabled) {
          setStatus("signed-in");
          return;
        }
        if (!isRuntimeConfigReady(loaded)) {
          setStatus("unavailable");
          return;
        }
        const pending = takePending();
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
        } else {
          forgetPending();
        }
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
    if (!isRuntimeConfigReady(config)) return;
    const verifier = await generateCodeVerifier();
    const challenge = await challengeFor(verifier);
    const state = generateState();
    rememberPending(verifier, state);
    window.location.assign(
      authorizeUrl(config, {
        redirectUri: redirectUriFor(window.location.origin),
        state,
        codeChallenge: challenge,
      }),
    );
  }, [config]);

  const signOut = useCallback(async () => {
    forgetPending();
    await logout(config);
    setStatus("signed-out");
  }, [config]);

  const value = useMemo<AuthContextValue>(
    () => ({ status, config, signIn, signOut }),
    [status, config, signIn, signOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

// The portal's session: who is signed in, and how the connection gets a
// credential (memql#3315).
//
// ONE provider, mounted above the router's routes, owning:
//   * the runtime config read from the serving node (which identity service),
//   * the session state machine, and
//   * the single PortalAuthSource handed to <ClusterProvider>.
//
// The auth source's OBJECT IDENTITY is deliberately stable for the life of
// the provider. ClusterProvider re-dials whenever its `auth` prop changes
// identity, so a source rebuilt on every render would tear the WebSocket down
// on every render -- and a token rotation, which is the thing this whole file
// exists to make invisible, would become a reconnect.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import {
  UNKNOWN_RUNTIME_CONFIG,
  loadRuntimeConfig,
  type PortalRuntimeConfig,
} from "../cluster/config";
import { portalRedirectPath } from "../cluster/endpoint";
import {
  createIdentityAuthSource,
  type IdentityAuthSource,
} from "./identityAuthSource";
import {
  authorizeUrl,
  endIdentitySession,
  exchangeAuthorizationCode,
  IdentityHttpError,
  type IdentityFetch,
} from "./identityClient";
import { createPkcePair, newOAuthState, type CryptoLike } from "./pkce";
import {
  consumePending,
  savePending,
  safeReturnTo,
  DEFAULT_RETURN_TO,
  type StorageLike,
} from "./pending";
import type { PortalAuthSource } from "../cluster/auth";

export type AuthStatus =
  // Reading the runtime config / probing for an existing session. Nothing is
  // rendered underneath yet, because "signed out" and "not yet known" must
  // not look the same -- flashing a sign-in page at an operator who IS signed
  // in is how a console teaches people to distrust it.
  | "loading"
  // No session. The sign-in view is shown in place of the requested route.
  | "signedOut"
  // A credential is held; the cluster connection may dial.
  | "signedIn"
  // This cluster runs with MEMQL_IDENTITY_ENABLED=false. There is no sign-in
  // to perform, and the stream admits every dial as the synthetic local-dev
  // cluster owner. Treated as usable, and SAID SO in the header.
  | "authDisabled"
  // The portal cannot sign anyone in on this cluster -- no identity URL, no
  // client id, or the runtime config would not load. Distinct from signedOut
  // because the remedy is an operator's, not the user's.
  | "misconfigured";

export interface AuthState {
  status: AuthStatus;
  config: PortalRuntimeConfig;
  // The credential source for <ClusterProvider>. Stable identity.
  authSource: PortalAuthSource;
  // Non-empty when something went wrong the operator needs to read.
  error: string;
  // Start the OAuth flow, remembering `returnTo` across the redirect.
  signIn: (returnTo?: string) => void;
  // Drop the local credential and clear the refresh cookie.
  signOut: () => void;
  // Finish the flow. Called only by the callback route.
  completeSignIn: (params: {
    code: string;
    state: string;
  }) => Promise<{ ok: boolean; returnTo: string; error: string }>;
  // True while the session is usable -- i.e. the connection may dial.
  connectable: boolean;
}

const AuthContext = createContext<AuthState | null>(null);

export interface AuthProviderProps {
  children: ReactNode;
  // Injection points. Production passes none of these; every one exists so
  // the tests can drive the flow without a network, a real crypto
  // implementation, or a real navigation.
  fetchImpl?: IdentityFetch;
  storage?: StorageLike | null;
  cryptoImpl?: CryptoLike;
  // Performs the top-level navigation to the identity service. Overridden in
  // tests because jsdom cannot navigate.
  navigate?: (url: string) => void;
  // Overrides the runtime config, skipping the fetch entirely.
  config?: PortalRuntimeConfig;
  // The absolute redirect_uri registered with identity. Derived from the
  // document origin in production.
  redirectUri?: string;
}

function defaultFetch(input: string, init?: RequestInit): Promise<Response> {
  return globalThis.fetch(input, init);
}

function defaultNavigate(url: string): void {
  // assign, not replace: the sign-in page stays in history, so a browser
  // "back" from the identity service returns somewhere coherent rather than
  // skipping past the portal entirely.
  globalThis.location.assign(url);
}

// defaultRedirectUri is the absolute callback URL. It must equal, byte for
// byte, a redirect_uri registered for this client on the identity service --
// component/identity/config.go matches by exact string, so a trailing slash
// or a missing port is a 400 at /authorize with "Invalid redirect URI".
function defaultRedirectUri(): string {
  const origin = globalThis.location?.origin ?? "";
  return origin ? origin + portalRedirectPath : "";
}

export function AuthProvider({
  children,
  fetchImpl = defaultFetch,
  storage,
  cryptoImpl,
  navigate = defaultNavigate,
  config: configOverride,
  redirectUri = defaultRedirectUri(),
}: AuthProviderProps): ReactNode {
  const [status, setStatus] = useState<AuthStatus>("loading");
  const [config, setConfig] = useState<PortalRuntimeConfig>(
    configOverride ?? UNKNOWN_RUNTIME_CONFIG,
  );
  const [error, setError] = useState("");

  // The runtime config is read once and then held in a ref as well as state:
  // the auth source and the sign-in callbacks are built ONCE (stable
  // identity, see the file header) and must see the current config without
  // being rebuilt when it arrives.
  const configRef = useRef<PortalRuntimeConfig>(configOverride ?? UNKNOWN_RUNTIME_CONFIG);

  // Guards a settle from a superseded run. StrictMode double-mounts effects in
  // development, and the bootstrap probe is async.
  const generation = useRef(0);

  const authSourceRef = useRef<IdentityAuthSource | null>(null);
  if (authSourceRef.current === null) {
    authSourceRef.current = createIdentityAuthSource({
      // Read through the ref so the source, built once, always sees the
      // config that eventually loaded. Rebuilding the source when the config
      // lands would change its identity and re-dial the cluster connection.
      config: () => configRef.current,
      fetchImpl,
      onSessionEnded: () => {
        // Reactive sign-out: identity says the session is gone (revoked
        // elsewhere, refresh expired). Distinct from a transient failure,
        // which createIdentityAuthSource deliberately does not report here.
        setStatus((current) => (current === "authDisabled" ? current : "signedOut"));
      },
    });
  }
  const authSource = authSourceRef.current;

  // ---- bootstrap: read the config, then probe for an existing session -----
  useEffect(() => {
    const mine = ++generation.current;
    let cancelled = false;

    void (async () => {
      let resolved = configOverride;
      if (!resolved) {
        try {
          resolved = await loadRuntimeConfig(fetchImpl);
        } catch (err) {
          if (cancelled || generation.current !== mine) return;
          configRef.current = UNKNOWN_RUNTIME_CONFIG;
          setStatus("misconfigured");
          setError(err instanceof Error ? err.message : String(err));
          return;
        }
      }
      if (cancelled || generation.current !== mine) return;

      configRef.current = resolved;
      setConfig(resolved);

      if (!resolved.authEnabled) {
        // Auth is off on this cluster. Do not probe for a session that cannot
        // exist, and do not show a sign-in the operator cannot complete.
        setStatus("authDisabled");
        return;
      }
      if (!resolved.identityUrl || !resolved.oauthClientId) {
        setStatus("misconfigured");
        setError(
          "This cluster enforces authentication but published no identity URL " +
            "or OAuth client id, so the portal cannot sign anyone in. See " +
            "docs/public/operate/portal.md.",
        );
        return;
      }

      // Probe for an existing session by attempting a refresh.
      //
      // ONE unconditional request, and a 401 is a NORMAL outcome. identity
      // sets a JS-readable `memql_session` marker cookie next to the HttpOnly
      // refresh cookie precisely so a SPA can skip this probe -- but that
      // cookie is set on the IDENTITY origin and the portal is served from the
      // bff's, so document.cookie here cannot see it. Probing unconditionally
      // is the honest cost of the cross-origin topology; it is one request per
      // cold load.
      const token = await authSource.refresh();
      if (cancelled || generation.current !== mine) return;
      setStatus(token ? "signedIn" : "signedOut");
    })();

    return () => {
      cancelled = true;
    };
    // configOverride/fetchImpl are test-injected and constant in production.
  }, [authSource, configOverride, fetchImpl]);

  const signIn = useCallback(
    (returnTo?: string) => {
      const destination = safeReturnTo(returnTo ?? currentInAppPath());
      void (async () => {
        try {
          const cfg = configRef.current;
          const pkce = await createPkcePair(cryptoImpl);
          const state = newOAuthState(cryptoImpl);
          const url = authorizeUrl(cfg, {
            redirectUri,
            state,
            codeChallenge: pkce.challenge,
          });
          // Persist BEFORE navigating. If storage is unavailable the callback
          // would arrive with no verifier and no state to check, which
          // presents as a sign-in that silently loops -- so refuse up front
          // and say why.
          const saved = savePending(
            { state, verifier: pkce.verifier, returnTo: destination, createdAt: Date.now() },
            storage,
          );
          if (!saved) {
            setError(
              "Sign-in needs session storage, which this browser has blocked " +
                "for this site. Allow storage for this origin and try again.",
            );
            return;
          }
          setError("");
          navigate(url);
        } catch (err) {
          setError(err instanceof Error ? err.message : String(err));
        }
      })();
    },
    [cryptoImpl, navigate, redirectUri, storage],
  );

  const completeSignIn = useCallback(
    async ({ code, state }: { code: string; state: string }) => {
      const pending = consumePending(storage);
      if (!pending) {
        return {
          ok: false,
          returnTo: DEFAULT_RETURN_TO,
          error:
            "This sign-in could not be matched to a request from this tab. " +
            "It may have expired, or the link may have been opened in a " +
            "different browser. Start again from the portal.",
        };
      }
      // CSRF check (OAuth 2.0 §10.12). Without it an attacker can hand the
      // portal an authorization code of their own and sign the operator into
      // the ATTACKER's account, which is a far quieter compromise than the
      // reverse.
      if (state !== pending.state) {
        return {
          ok: false,
          returnTo: pending.returnTo,
          error: "Sign-in state did not match. The response was discarded.",
        };
      }
      try {
        const tokens = await exchangeAuthorizationCode(fetchImpl, configRef.current, {
          code,
          codeVerifier: pending.verifier,
          redirectUri,
        });
        authSource.adopt(tokens.accessToken);
        setError("");
        setStatus("signedIn");
        return { ok: true, returnTo: pending.returnTo, error: "" };
      } catch (err) {
        const message =
          err instanceof IdentityHttpError
            ? `${err.message}${err.code ? ` (${err.code})` : ""}`
            : err instanceof Error
              ? err.message
              : String(err);
        setStatus("signedOut");
        return { ok: false, returnTo: pending.returnTo, error: message };
      }
    },
    [authSource, fetchImpl, redirectUri, storage],
  );

  const signOut = useCallback(() => {
    // Local first. The operator's intent is satisfied the moment this page
    // stops holding a credential; the server round-trip that clears the
    // refresh cookie is best-effort behind it.
    authSource.forget();
    setStatus((current) => (current === "authDisabled" ? current : "signedOut"));
    void endIdentitySession(fetchImpl, configRef.current);
  }, [authSource, fetchImpl]);

  const value = useMemo<AuthState>(
    () => ({
      status,
      config,
      authSource,
      error,
      signIn,
      signOut,
      completeSignIn,
      // NOT A PERMISSION CHECK. This only decides whether to open a WebSocket;
      // what the resulting stream may read or write is decided server-side by
      // the verifier interceptors in component/grpc. Nothing in the portal
      // gates data on a client-side boolean.
      connectable: status === "signedIn" || status === "authDisabled",
    }),
    [status, config, authSource, error, signIn, signOut, completeSignIn],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

// useAuth is the only sanctioned way to reach the session. Throws on a missing
// provider for the same reason useCluster does: a null-shaped default lets a
// mis-mounted component render a permanently-signed-out page instead of
// failing where the mistake is.
export function useAuth(): AuthState {
  const state = useContext(AuthContext);
  if (state === null) {
    throw new Error("useAuth must be used inside <AuthProvider>");
  }
  return state;
}

// currentInAppPath is the router-relative path of the current location --
// what the operator typed or clicked, minus the deployment's mount prefix, so
// it can be handed straight back to react-router after the round trip.
function currentInAppPath(): string {
  const loc = globalThis.location;
  if (!loc) return DEFAULT_RETURN_TO;
  const mount = import.meta.env.BASE_URL.replace(/\/+$/, "");
  const full = loc.pathname + loc.search + loc.hash;
  const relative = mount && full.startsWith(mount) ? full.slice(mount.length) : full;
  return safeReturnTo(relative || DEFAULT_RETURN_TO);
}

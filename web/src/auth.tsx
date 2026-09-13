import { createContext, useCallback, useContext, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";
import { ApiClient, ApiError } from "./api/client";

const TOKEN_KEY = "omahab.session";
function consumeFragmentToken(): void {
  try {
    if (typeof window === "undefined" || !window.location) return;
    const loc = window.location;
    const rawHash = loc.hash;
    if (!rawHash || rawHash.length <= 1) return;
    const raw = rawHash.slice(1);
    if (!raw) return;
    const parts = raw.split("&");
    let found = false;
    let tokenValue: string | null = null;
    const remaining: string[] = [];
    for (const part of parts) {
      if (part === "") continue;
      const eqIdx = part.indexOf("=");
      let keyRaw: string;
      let valueRaw: string;
      if (eqIdx === -1) {
        keyRaw = part;
        valueRaw = "";
      } else {
        keyRaw = part.slice(0, eqIdx);
        valueRaw = part.slice(eqIdx + 1);
      }
      let key: string;
      try {
        key = decodeURIComponent(keyRaw);
      } catch {
        key = keyRaw;
      }
      if (key === "token") {
        found = true;
        if (tokenValue === null) {
          let decoded: string;
          try {
            decoded = decodeURIComponent(valueRaw);
          } catch {
            decoded = valueRaw;
          }
          const trimmed = decoded.trim();
          if (trimmed) tokenValue = trimmed;
        }
        continue;
      }
      remaining.push(part);
    }
    if (!found) return;
    if (tokenValue) {
      try {
        sessionStorage.setItem(TOKEN_KEY, tokenValue);
      } catch {}
    }
    const newHash = remaining.length ? `#${remaining.join("&")}` : "";
    const newUrl = `${loc.pathname}${loc.search}${newHash}`;
    const currentSuffix = `${loc.pathname}${loc.search}${loc.hash}`;
    if (newUrl !== currentSuffix) {
      try {
        if (typeof history !== "undefined" && typeof history.replaceState === "function") {
          history.replaceState(history.state, "", newUrl);
        }
      } catch {}
    }
  } catch {}
}

interface AuthContextValue {
  token: string | null;
  // True when the server accepts this browser without a token (lan placement
  // + LAN or tailnet source address). Null while the probe is in flight.
  lanBypass: boolean | null;
  client: ApiClient;
  authError: string | null;
  clearAuthError: () => void;
  signIn: (token: string) => void;
  signOut: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState<string | null>(() => {
    consumeFragmentToken();
    try {
      return sessionStorage.getItem(TOKEN_KEY);
    } catch {
      return null;
    }
  });
  const [authError, setAuthError] = useState<string | null>(null);
  const clearAuthError = useCallback(() => setAuthError(null), []);
  const signIn = useCallback((value: string) => {
    try {
      sessionStorage.setItem(TOKEN_KEY, value);
    } catch {}
    setToken(value);
    setAuthError(null);
  }, []);
  const signOut = useCallback(() => {
    try {
      sessionStorage.removeItem(TOKEN_KEY);
    } catch {}
    setToken(null);
  }, []);
  const client = useMemo(() => new ApiClient(() => {
    try {
      return sessionStorage.getItem(TOKEN_KEY);
    } catch {
      return null;
    }
  }), []);

  // LAN bypass probe: without a token, GET /api/v1/status succeeds only when
  // the server trusts this source address (lan placement + LAN or tailnet). Raw fetch on
  // purpose — a 401 here must not fire the global unauthorized handler.
  const [lanBypass, setLanBypass] = useState<boolean | null>(null);
  useEffect(() => {
    if (token) {
      setLanBypass(null);
      return;
    }
    let cancelled = false;
    setLanBypass(null);
    (async () => {
      try {
        const res = await fetch("/api/v1/status", { cache: "no-store" });
        if (!cancelled) setLanBypass(res.ok);
      } catch {
        if (!cancelled) setLanBypass(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [token]);

  useEffect(() => {
    function handleUnauthorized() {
      let enrolled = false;
      try {
        enrolled = sessionStorage.getItem(TOKEN_KEY) !== null;
      } catch {}
      setAuthError(enrolled
        ? "Your session has expired or the token is invalid. Please sign in again."
        : "Sign in with the 8-character panel token to continue.");
      signOut();
    }
    window.addEventListener("omahab:unauthorized", handleUnauthorized);
    return () => window.removeEventListener("omahab:unauthorized", handleUnauthorized);
  }, [signOut]);

  const value = useMemo(() => ({ token, lanBypass, client, authError, clearAuthError, signIn, signOut }), [client, signIn, signOut, token, lanBypass, authError, clearAuthError]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used within AuthProvider");
  return value;
}

export function ProtectedRoute({ children }: { children: ReactNode }) {
  const { token, lanBypass } = useAuth();
  const location = useLocation();
  if (token || lanBypass) return <>{children}</>;
  if (lanBypass === null) {
    return (
      <div className="state-message" role="status" style={{ minHeight: "60vh", display: "flex", alignItems: "center", justifyContent: "center" }}>
        <span className="spinner" aria-hidden="true" /> Checking access…
      </div>
    );
  }
  return <Navigate to="/login" replace state={{ from: location.pathname }} />;
}

export function LoginPage() {
  const { token, lanBypass, signIn, authError, clearAuthError } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const destination = (location.state as { from?: string } | null)?.from ?? "/";
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [validating, setValidating] = useState(false);
  const displayError = submitError ?? authError;

  // LAN bypass: no token needed when the server trusts this address. While the
  // probe is in flight there is nothing to sign into yet — show waiting state
  // instead of a token form that LAN mode never needs.
  if (token || lanBypass) return <Navigate to={destination} replace />;
  if (lanBypass === null) {
    return (
      <main className="login-page">
        <section className="login-card" aria-labelledby="login-title">
          <h1 id="login-title">Sign in to Omahab</h1>
          <div className="state-message" role="status">
            <span className="spinner" aria-hidden="true" /> Checking access…
          </div>
        </section>
      </main>
    );
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const value = String(form.get("token") ?? "").trim();
    if (!value) return;
    setSubmitError(null);
    clearAuthError();
    setValidating(true);
    try {
      const probe = new ApiClient(() => value);
      await probe.status();
      signIn(value);
      navigate(destination, { replace: true });
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setSubmitError("Invalid or expired token. Please check the token and try again.");
      } else if (error instanceof ApiError) {
        setSubmitError(error.message);
      } else if (error instanceof Error) {
        setSubmitError(error.message);
      } else {
        setSubmitError("Sign in failed. Please try again.");
      }
    } finally {
      setValidating(false);
    }
  }

  return (
    <main className="login-page">
      <section className="login-card" aria-labelledby="login-title">
        <h1 id="login-title">Sign in to Omahab</h1>
        <p className="muted">Enter the 8-character panel token shown on the server console. It remains in this browser tab only. On a home-LAN install no token is needed, on the LAN or the tailnet.</p>
        {displayError && <p className="inline-error" role="alert">{displayError}</p>}
        <p className="muted">Find it on the host via <code className="mono">sudo cat /var/lib/omahab/api.token</code>, or reuse the token at <code className="mono">~/.config/omahab/token</code>.</p>
        <form onSubmit={submit} className="form-stack">
          <label>
            Access token
            <input name="token" type="password" autoComplete="off" required autoFocus />
          </label>
          <button className="button primary" type="submit" disabled={validating}>{validating ? "Verifying…" : "Continue"}</button>
        </form>
      </section>
    </main>
  );
}

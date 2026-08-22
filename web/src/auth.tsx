import { createContext, useCallback, useContext, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";
import { ApiClient, ApiError } from "./api/client";

const TOKEN_KEY = "omahab.session";

interface AuthContextValue {
  token: string | null;
  client: ApiClient;
  authError: string | null;
  clearAuthError: () => void;
  signIn: (token: string) => void;
  signOut: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState<string | null>(() => sessionStorage.getItem(TOKEN_KEY));
  const [authError, setAuthError] = useState<string | null>(null);
  const clearAuthError = useCallback(() => setAuthError(null), []);
  const signIn = useCallback((value: string) => {
    sessionStorage.setItem(TOKEN_KEY, value);
    setToken(value);
    setAuthError(null);
  }, []);
  const signOut = useCallback(() => {
    sessionStorage.removeItem(TOKEN_KEY);
    setToken(null);
  }, []);
  const client = useMemo(() => new ApiClient(() => sessionStorage.getItem(TOKEN_KEY)), []);

  useEffect(() => {
    function handleUnauthorized() {
      setAuthError("Your session has expired or the token is invalid. Please sign in again.");
      signOut();
    }
    window.addEventListener("omahab:unauthorized", handleUnauthorized);
    return () => window.removeEventListener("omahab:unauthorized", handleUnauthorized);
  }, [signOut]);

  const value = useMemo(() => ({ token, client, authError, clearAuthError, signIn, signOut }), [client, signIn, signOut, token, authError, clearAuthError]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used within AuthProvider");
  return value;
}

export function ProtectedRoute({ children }: { children: ReactNode }) {
  const { token } = useAuth();
  const location = useLocation();
  if (!token) return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  return children;
}

export function LoginPage() {
  const { token, signIn, authError, clearAuthError } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const destination = (location.state as { from?: string } | null)?.from ?? "/";
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [validating, setValidating] = useState(false);
  const displayError = submitError ?? authError;

  if (token) return <Navigate to={destination} replace />;

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
        <p className="eyebrow">Private control plane</p>
        <h1 id="login-title">Sign in to Omahab</h1>
        <p className="muted">Use a short-lived bearer credential issued by your Omahab administrator. It remains in this browser tab only.</p>
        {displayError && <p className="inline-error" role="alert">{displayError}</p>}
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

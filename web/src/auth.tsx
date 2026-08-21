import { createContext, useCallback, useContext, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";
import { ApiClient } from "./api/client";

const TOKEN_KEY = "omahab.session";

interface AuthContextValue {
  token: string | null;
  client: ApiClient;
  signIn: (token: string) => void;
  signOut: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState<string | null>(() => sessionStorage.getItem(TOKEN_KEY));
  const signIn = useCallback((value: string) => {
    sessionStorage.setItem(TOKEN_KEY, value);
    setToken(value);
  }, []);
  const signOut = useCallback(() => {
    sessionStorage.removeItem(TOKEN_KEY);
    setToken(null);
  }, []);
  const client = useMemo(() => new ApiClient(() => sessionStorage.getItem(TOKEN_KEY)), []);

  useEffect(() => {
    window.addEventListener("omahab:unauthorized", signOut);
    return () => window.removeEventListener("omahab:unauthorized", signOut);
  }, [signOut]);

  const value = useMemo(() => ({ token, client, signIn, signOut }), [client, signIn, signOut, token]);
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
  const { token, signIn } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const destination = (location.state as { from?: string } | null)?.from ?? "/";

  if (token) return <Navigate to={destination} replace />;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const value = String(form.get("token") ?? "").trim();
    if (!value) return;
    signIn(value);
    navigate(destination, { replace: true });
  }

  return (
    <main className="login-page">
      <section className="login-card" aria-labelledby="login-title">
        <p className="eyebrow">Private control plane</p>
        <h1 id="login-title">Sign in to Omahab</h1>
        <p className="muted">Use a short-lived bearer credential issued by your Omahab administrator. It remains in this browser tab only.</p>
        <form onSubmit={submit} className="form-stack">
          <label>
            Access token
            <input name="token" type="password" autoComplete="off" required autoFocus />
          </label>
          <button className="button primary" type="submit">Continue</button>
        </form>
      </section>
    </main>
  );
}

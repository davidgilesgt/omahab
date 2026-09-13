import { useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { LoginPage, ProtectedRoute } from "./auth";
import { AppShell } from "./components/shell";
import { ApplicationsPage, BackupsPage, EventsPage, OverviewPage, ProjectsPage } from "./views/operations";
import { PeoplePage, SyncFoldersPage, WorkspacesPage } from "./views/administration";
import { ToolEnvironmentPage } from "./views/tool-environment";
import { DevicesPage } from "./views/devices";
import { AssistantKnowledgePanel } from "./views/knowledge";
import { DoctorPage } from "./views/doctor";
import { HomePage } from "./views/home";
import { WelcomePage } from "./views/welcome";
import { SetupPage } from "./views/setup";

function isHomeHost(): boolean {
  if (typeof window === "undefined") return false;
  const h = window.location.hostname;
  if (!h) return false;
  if (h === "home" || h.startsWith("home.")) return true;
  if (h.includes(".home.")) return true;
  return false;
}

function DashboardRoutes() {
  return (
    <ProtectedRoute>
      <AppShell>
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/setup" element={<SetupPage />} />
          <Route path="/applications" element={<ApplicationsPage />} />
          <Route path="/projects" element={<ProjectsPage />} />
          <Route path="/backups" element={<BackupsPage />} />
          <Route path="/events" element={<EventsPage />} />
          <Route path="/sync" element={<SyncFoldersPage />} />
          <Route path="/workspaces" element={<WorkspacesPage />} />
          <Route path="/people" element={<PeoplePage />} />
          <Route path="/tool-environment" element={<ToolEnvironmentPage />} />
          <Route path="/devices" element={<DevicesPage />} />
          <Route path="/ai" element={<AssistantKnowledgePanel />} />
          <Route path="/doctor" element={<DoctorPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </AppShell>
    </ProtectedRoute>
  );
}

function AdminDashboardRoutes() {
  return (
    <ProtectedRoute>
      <AppShell basePath="/admin">
        <Routes>
          <Route index element={<OverviewPage />} />
          <Route path="setup" element={<SetupPage />} />
          <Route path="applications" element={<ApplicationsPage />} />
          <Route path="projects" element={<ProjectsPage />} />
          <Route path="backups" element={<BackupsPage />} />
          <Route path="events" element={<EventsPage />} />
          <Route path="sync" element={<SyncFoldersPage />} />
          <Route path="workspaces" element={<WorkspacesPage />} />
          <Route path="people" element={<PeoplePage />} />
          <Route path="tool-environment" element={<ToolEnvironmentPage />} />
          <Route path="devices" element={<DevicesPage />} />
          <Route path="ai" element={<AssistantKnowledgePanel />} />
          <Route path="doctor" element={<DoctorPage />} />
          <Route path="*" element={<Navigate to="/admin/" replace />} />
        </Routes>
      </AppShell>
    </ProtectedRoute>
  );
}

function HomeHostRedirects() {
  const location = useLocation();
  const path = location.pathname;
  const dashboardPaths = [
    "/setup",
    "/applications",
    "/projects",
    "/backups",
    "/events",
    "/sync",
    "/workspaces",
    "/people",
    "/tool-environment",
    "/devices",
    "/ai",
    "/doctor",
  ];
  for (const dp of dashboardPaths) {
    if (path === dp || path.startsWith(dp + "/")) {
      return <Navigate to={`/admin${path}`} replace />;
    }
  }
  return <HomePage />;
}

export default function App() {
  const [serverError, setServerError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const fetchStatus = useCallback(async () => {
    setLoading(true);
    setServerError(null);
    try {
      const res = await fetch("/up", { cache: "no-store" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
    } catch (e) {
      const msg = e instanceof Error ? e.message : "Failed to reach server";
      setServerError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchStatus();
  }, [fetchStatus]);

  if (loading) {
    return (
      <div className="state-message" role="status" style={{ minHeight: "60vh", display: "flex", alignItems: "center", justifyContent: "center" }}>
        <span className="spinner" aria-hidden="true" /> Checking server status…
      </div>
    );
  }

  if (serverError !== null) {
    return (
      <div
        className="state-message error-state"
        role="alert"
        style={{ minHeight: "60vh", display: "flex", flexDirection: "column", alignItems: "center", justifyContent: "center", gap: 12, padding: 24, textAlign: "center" }}
      >
        <div>
          <strong>Can't reach the server</strong>
          <p>{serverError}</p>
          <p className="muted" style={{ fontSize: "0.9em" }}>Check that the server is on and on the same network, then retry.</p>
        </div>
        <button className="button primary" type="button" onClick={() => void fetchStatus()}>
          Retry
        </button>
      </div>
    );
  }

  // No claim step: host-aware dashboard routing, redirect deep link.
  const isHome = isHomeHost();
  if (isHome) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/bootstrap" element={<Navigate to="/" replace />} />
        <Route path="/welcome/:token" element={<WelcomePage />} />
        <Route path="/admin/*" element={<AdminDashboardRoutes />} />
        <Route path="/admin" element={<Navigate to="/admin/" replace />} />
        <Route path="/" element={<HomePage />} />
        <Route path="/*" element={<HomeHostRedirects />} />
      </Routes>
    );
  }
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/bootstrap" element={<Navigate to="/" replace />} />
      <Route path="/welcome/:token" element={<WelcomePage />} />
      <Route path="/admin" element={<Navigate to="/admin/" replace />} />
      <Route path="/admin/*" element={<AdminDashboardRoutes />} />
      <Route path="/*" element={<DashboardRoutes />} />
    </Routes>
  );
}

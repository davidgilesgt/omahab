import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { LoginPage, ProtectedRoute } from "./auth";
import { AppShell } from "./components/shell";
import { ApplicationsPage, BackupsPage, EventsPage, OverviewPage, ProjectsPage } from "./views/operations";
import { PeoplePage, ProvidersPage, SyncFoldersPage, WorkspacesPage } from "./views/administration";
import { ToolEnvironmentPage } from "./views/tool-environment";
import { DevicesPage } from "./views/devices";
import { AssistantKnowledgePanel } from "./views/knowledge";
import { DoctorPage } from "./views/doctor";
import { HomePage } from "./views/home";
import { WelcomePage } from "./views/welcome";
import { BootstrapPage } from "./views/bootstrap";
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
          <Route path="/providers" element={<ProvidersPage />} />
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
          <Route path="/admin/" element={<OverviewPage />} />
          <Route path="/admin/setup" element={<SetupPage />} />
          <Route path="/admin/applications" element={<ApplicationsPage />} />
          <Route path="/admin/projects" element={<ProjectsPage />} />
          <Route path="/admin/backups" element={<BackupsPage />} />
          <Route path="/admin/events" element={<EventsPage />} />
          <Route path="/admin/sync" element={<SyncFoldersPage />} />
          <Route path="/admin/workspaces" element={<WorkspacesPage />} />
          <Route path="/admin/people" element={<PeoplePage />} />
          <Route path="/admin/providers" element={<ProvidersPage />} />
          <Route path="/admin/tool-environment" element={<ToolEnvironmentPage />} />
          <Route path="/admin/devices" element={<DevicesPage />} />
          <Route path="/admin/ai" element={<AssistantKnowledgePanel />} />
          <Route path="/admin/doctor" element={<DoctorPage />} />
          <Route path="/admin/*" element={<Navigate to="/admin/" replace />} />
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
    "/providers",
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
  const isHome = isHomeHost();
  if (isHome) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/bootstrap" element={<BootstrapPage />} />
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
      <Route path="/bootstrap" element={<BootstrapPage />} />
      <Route path="/welcome/:token" element={<WelcomePage />} />
      <Route path="/admin/*" element={<AdminDashboardRoutes />} />
      <Route path="/*" element={<DashboardRoutes />} />
    </Routes>
  );
}

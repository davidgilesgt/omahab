import { Navigate, Route, Routes } from "react-router-dom";
import { LoginPage, ProtectedRoute } from "./auth";
import { AppShell } from "./components/shell";
import { ApplicationsPage, BackupsPage, EventsPage, OverviewPage, ProjectsPage } from "./views/operations";
import { PeoplePage, ProvidersPage, SyncFoldersPage, WorkspacesPage } from "./views/administration";
import { ToolEnvironmentPage } from "./views/tool-environment";
import { ChatPage } from "./views/chat";
import { SetupPage } from "./views/setup";
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
          <Route path="/ai" element={<ChatPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </AppShell>
    </ProtectedRoute>
  );
}

export default function App() {
  return <Routes><Route path="/login" element={<LoginPage />} /><Route path="*" element={<DashboardRoutes />} /></Routes>;
}

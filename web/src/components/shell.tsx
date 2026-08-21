import { useEffect, useRef, useState, type ChangeEvent, type ReactNode } from "react";
import { NavLink, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "../auth";

const NAVIGATION = [
  ["/", "Overview", "⌂"],
  ["/applications", "Applications", "▦"],
  ["/projects", "Projects", "⌘"],
  ["/backups", "Backups", "↺"],
  ["/events", "Inbox", "●"],
  ["/sync", "Sync folders", "⇄"],
  ["/workspaces", "Workspaces", "◇"],
  ["/people", "People & access", "◎"],
  ["/providers", "Providers", "◐"],
  ["/ai", "AI", "✦"],
] as const;

export function AppShell({ children }: { children: ReactNode }) {
  const { client, signOut } = useAuth();
  const navigate = useNavigate();
  const searchRef = useRef<HTMLInputElement>(null);
  const [theme, setTheme] = useState(() => localStorage.getItem("omahab.theme") ?? "system");
  const eventQuery = useQuery({ queryKey: ["events"], queryFn: client.events, staleTime: 30_000 });
  const unread = eventQuery.data?.filter((event) => !event.read_at).length ?? 0;

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("omahab.theme", theme);
  }, [theme]);

  useEffect(() => {
    function handleShortcut(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        searchRef.current?.focus();
      }
    }
    window.addEventListener("keydown", handleShortcut);
    return () => window.removeEventListener("keydown", handleShortcut);
  }, []);

  function chooseDestination(event: ChangeEvent<HTMLInputElement>) {
    const match = NAVIGATION.find(([, label]) => label.toLowerCase() === event.currentTarget.value.toLowerCase());
    if (match) {
      navigate(match[0]);
      event.currentTarget.value = "";
      event.currentTarget.blur();
    }
  }

  return (
    <div className="app-frame">
      <a href="#main-content" className="skip-link">Skip to content</a>
      <aside className="sidebar">
        <NavLink to="/" className="brand" aria-label="Omahab overview">
          <span className="brand-mark">O</span>
          <span><strong>Omahab</strong><small>Control plane</small></span>
        </NavLink>
        <nav aria-label="Primary navigation">
          {NAVIGATION.map(([to, label, icon]) => (
            <NavLink key={to} to={to} end={to === "/"} className={({ isActive }) => isActive ? "nav-link active" : "nav-link"}>
              <span aria-hidden="true" className="nav-icon">{icon}</span>
              <span>{label}</span>
              {to === "/events" && unread > 0 && <span className="nav-badge" aria-label={`${unread} unread`}>{unread}</span>}
            </NavLink>
          ))}
        </nav>
        <div className="sidebar-footer">
          <span className="privacy-indicator"><span aria-hidden="true" /> Private by default</span>
          <button type="button" className="text-button" onClick={signOut}>Sign out</button>
        </div>
      </aside>
      <div className="content-frame">
        <header className="topbar">
          <label className="quick-nav">
            <span className="sr-only">Go to a page</span>
            <input ref={searchRef} list="destinations" placeholder="Go to…" onChange={chooseDestination} />
            <kbd>Ctrl K</kbd>
            <datalist id="destinations">
              {NAVIGATION.map(([, label]) => <option key={label} value={label} />)}
            </datalist>
          </label>
          <label className="theme-control">
            <span className="sr-only">Color theme</span>
            <select value={theme} onChange={(event) => setTheme(event.currentTarget.value)} aria-label="Color theme">
              <option value="system">System theme</option>
              <option value="light">Light theme</option>
              <option value="dark">Dark theme</option>
            </select>
          </label>
        </header>
        <main id="main-content" tabIndex={-1}>{children}</main>
      </div>
    </div>
  );
}

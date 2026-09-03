import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { NavLink, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useEventStream } from "./useEventStream";
const NAVIGATION = [
  ["/", "Overview", "⌂", "Home overview"],
  ["/setup", "Setup", "✓", "Continue setup"],
  ["/applications", "Services", "▦", "Platform services"],
  ["/projects", "Projects", "⌘", "ONCE projects"],
  ["/backups", "Backups", "↺", "Backup status"],
  ["/events", "Inbox", "●", "Event inbox"],
  ["/sync", "Sync folders", "⇄", "Syncthing folders"],
  ["/workspaces", "Workspaces", "◇", "Remote workspaces"],
  ["/people", "People & access", "◎", "Users and access"],
  ["/providers", "Providers", "◐", "External providers"],
  ["/tool-environment", "Tool environment", "⚙", "Agent tool variables and companion grants"],
] as const;

function aiDashboardUrl(): string {
  if (typeof window === "undefined") return "https://ai.example.com";
  const host = window.location.hostname;
  if (!host || host === "localhost" || host === "127.0.0.1") return "https://ai.example.com";
  const parts = host.split(".");
  if (parts.length < 2) return "https://" + host;
  // Replace first label with ai, or prepend ai if host looks like apex
  if (parts[0] === "ai") return window.location.protocol + "//" + host;
  // If host starts with www or omahab etc, replace first label with ai
  // For e.g., omahab.example.com -> ai.example.com ; dashboard.example.com -> ai.example.com
  // Keep suffix from second label onward if first label is not ai
  // Common case: <something>.<domain>.<tld> -> ai.<domain>.<tld>
  if (parts.length >= 3) {
    return window.location.protocol + "//ai." + parts.slice(1).join(".");
  }
  // apex domain like example.com -> ai.example.com
  return window.location.protocol + "//ai." + host;
}

export function AppShell({ children }: { children: ReactNode }) {
  const { client, signOut } = useAuth();
  const navigate = useNavigate();
  const searchRef = useRef<HTMLInputElement>(null);
  const [theme, setTheme] = useState(() => localStorage.getItem("omahab.theme") ?? "system");
  const [query, setQuery] = useState("");
  const [showResults, setShowResults] = useState(false);
  const eventQuery = useQuery({ queryKey: ["events"], queryFn: client.events, staleTime: Infinity });
  const unread = eventQuery.data?.filter((event) => !event.read_at).length ?? 0;
  const setupQuery = useQuery({ queryKey: ["setup"], queryFn: client.setup, retry: false, staleTime: 30_000 });
  const visibleNavigation = useMemo(() => {
    const state = setupQuery.data?.state;
    if (!state || state === "complete") return NAVIGATION.filter(([to]) => to !== "/setup");
    return [...NAVIGATION];
  }, [setupQuery.data]);
  useEventStream();
  // Keep theme in sync without flash - useLayoutEffect would be ideal but useEffect is okay with inline script
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("omahab.theme", theme);
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) {
      if (theme === "dark") meta.setAttribute("content", "#0f0f0f");
      else if (theme === "light") meta.setAttribute("content", "#f4f1e9");
      else meta.setAttribute("content", "#171815");
    }
  }, [theme]);


  useEffect(() => {
    function handleShortcut(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        searchRef.current?.focus();
        setShowResults(true);
      }
      if (event.key === "Escape") setShowResults(false);
    }
    window.addEventListener("keydown", handleShortcut);
    return () => window.removeEventListener("keydown", handleShortcut);
  }, []);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return [];
    return visibleNavigation.filter(([to, label]) =>
      label.toLowerCase().includes(q) || to.toLowerCase().includes(q)
    ).slice(0, 6);
  }, [query, visibleNavigation]);

  function chooseDestination(path: string) {
    navigate(path);
    setQuery("");
    setShowResults(false);
    searchRef.current?.blur();
  }

  return (
    <div className="app-frame">
      <a href="#main-content" className="skip-link" onClick={() => document.getElementById("main-content")?.focus()}>Skip to content</a>
      <aside className="sidebar">
        <NavLink to="/" className="brand" aria-label="Omahab overview">
          <span className="brand-mark">O</span>
          <span><strong>Omahab</strong><small>Control plane</small></span>
        </NavLink>
        <nav aria-label="Primary navigation">
          {visibleNavigation.map(([to, label, icon, title]) => (
            <NavLink key={to} to={to} end={to === "/"} className={({ isActive }) => isActive ? "nav-link active" : "nav-link"} title={title}>
              <span aria-hidden="true" className="nav-icon" title={title}>{icon}</span>
              <span>{label}</span>
              {to === "/events" && unread > 0 && <span className="nav-badge" aria-label={`${unread} unread`}>{unread}</span>}
            </NavLink>
          ))}
          <a href={aiDashboardUrl()} target="_blank" rel="noreferrer" className="nav-link" title="AI assistant (upstream Hermes dashboard)">
            <span aria-hidden="true" className="nav-icon" title="AI assistant (upstream Hermes dashboard)">✦</span>
            <span>AI</span>
          </a>
        </nav>
        <div className="sidebar-footer">
          <span className="privacy-indicator" aria-live="polite" title={unread > 0 ? `${unread} unread events` : "Private by default"}>
            <span aria-hidden="true" style={{ background: unread > 0 ? "var(--warning, #f59e0b)" : "var(--positive, #10b981)" }} />
            {unread > 0 ? `${unread} unread` : "Private by default"}
          </span>
          <button type="button" className="text-button" onClick={signOut}>Sign out</button>
        </div>
      </aside>
      <div className="content-frame">
        <header className="topbar">
          <div className="quick-nav" style={{ position: "relative" }}>
            <label className="sr-only" htmlFor="quick-nav-input">Go to a page</label>
            <input
              id="quick-nav-input"
              ref={searchRef}
              placeholder="Go to…"
              value={query}
              onChange={(e) => { setQuery(e.target.value); setShowResults(true); }}
              onFocus={() => setShowResults(true)}
              onBlur={() => setTimeout(() => setShowResults(false), 150)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && filtered.length > 0) {
                  e.preventDefault();
                  const first = filtered[0];
                  if (first) chooseDestination(first[0]);
                }
                if (e.key === "Escape") setShowResults(false);
              }}
              aria-autocomplete="list"
              aria-controls="quick-nav-listbox"
              aria-expanded={showResults && filtered.length > 0}
            />
            <kbd>Ctrl K</kbd>
            {showResults && filtered.length > 0 && (
              <ul id="quick-nav-listbox" role="listbox" style={{ position: "absolute", top: "100%", left: 0, right: 0, background: "var(--surface, #fff)", border: "1px solid var(--border)", borderRadius: 6, marginTop: 4, padding: 4, listStyle: "none", zIndex: 20 }}>
                {filtered.map(([to, label, icon]) => (
                  <li key={to} role="option" aria-selected={false} onMouseDown={(e) => { e.preventDefault(); chooseDestination(to); }} style={{ padding: "6px 8px", cursor: "pointer", display: "flex", gap: 8 }}>
                    <span aria-hidden>{icon}</span> {label} <small style={{ marginLeft: "auto", opacity: 0.6 }}>{to}</small>
                  </li>
                ))}
              </ul>
            )}
          </div>
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

import { useQuery } from "@tanstack/react-query";
import { Bookmark, FileText, FolderSync, GitBranch, Hammer, Image, Sparkles } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { useAuth } from "../auth";
import { ErrorState, LoadingState, Section, StatusPill } from "../components/ui";

type TileDef = {
  id: string;
  name: string;
  purpose: string;
  icon: LucideIcon;
  route: string;
  group: "everyday" | "build";
};

const TILES: TileDef[] = [
  { id: "immich", name: "Photos", purpose: "Private photo library", icon: Image, route: "photos", group: "everyday" },
  { id: "paperless-ngx", name: "Docs", purpose: "Document archive", icon: FileText, route: "docs", group: "everyday" },
  { id: "karakeep", name: "Save", purpose: "Bookmarks & saves", icon: Bookmark, route: "save", group: "everyday" },
  { id: "syncthing", name: "Sync", purpose: "File sync", icon: FolderSync, route: "sync", group: "everyday" },
  { id: "hermes", name: "AI", purpose: "Your AI assistant", icon: Sparkles, route: "ai", group: "everyday" },
  { id: "forgejo", name: "Git", purpose: "Code hosting", icon: GitBranch, route: "git", group: "build" },
  { id: "woodpecker", name: "CI", purpose: "Builds & deploys", icon: Hammer, route: "ci", group: "build" },
];

function getDomain(): string {
  if (typeof window === "undefined") return "";
  const host = window.location.hostname;
  if (!host) return "";
  if (host.startsWith("home.")) return host.slice(5);
  const idx = host.indexOf(".home.");
  if (idx !== -1) return host.slice(idx + 6);
  // fallback: strip first label if looks like home.<domain> variant
  const parts = host.split(".");
  if (parts.length >= 2) return parts.slice(1).join(".");
  return host;
}

function tileHref(tile: TileDef): string {
  const domain = getDomain();
  if (!domain) return `https://${tile.route}.example.com`;
  return `https://${tile.route}.${domain}`;
}

function HealthDot({ health }: { health: string }) {
  const normalized = health.toLowerCase();
  const color =
    normalized === "healthy" ? "var(--positive)" : normalized === "degraded" ? "var(--warning)" : "var(--negative)";
  return (
    <span
      aria-label={health}
      title={health}
      style={{
        display: "inline-block",
        width: 10,
        height: 10,
        borderRadius: "50%",
        background: color,
        boxShadow: `0 0 0 2px color-mix(in srgb, ${color} 20%, transparent)`,
        transition: "background 300ms ease, box-shadow 300ms ease",
        flexShrink: 0,
      }}
    />
  );
}

export function HomePage() {
  const { client } = useAuth();
  const query = useQuery({
    queryKey: ["public-status"],
    queryFn: client.publicStatus,
    refetchInterval: 15_000,
    retry: false,
  });

  const healthById = new Map<string, string>();
  if (query.data?.apps) {
    for (const app of query.data.apps) healthById.set(app.id, app.health);
  }

  const everyday = TILES.filter((t) => t.group === "everyday");
  const build = TILES.filter((t) => t.group === "build");

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <p className="eyebrow">Home</p>
          <h1>Welcome home</h1>
          <p>Your private apps — all on your tailnet, none on the public internet unless you choose.</p>
        </div>
        <div className="page-actions">
          <a className="button secondary" href="/admin">
            Admin
          </a>
        </div>
      </header>

      {query.isLoading ? (
        <LoadingState label="Checking services" />
      ) : query.isError ? (
        <ErrorState error={query.error} retry={() => void query.refetch()} />
      ) : null}

      <Section title="Everyday" description="Photos, documents, saves and sync — for everyone at home.">
        <div className="tile-grid">
          {everyday.map((tile) => {
            const h = healthById.get(tile.id) ?? "unknown";
            const Icon = tile.icon;
            return (
              <a key={tile.id} className="tile" href={tileHref(tile)} target="_blank" rel="noreferrer">
                <div className="tile-head">
                  <span className="tile-icon" aria-hidden="true">
                    <Icon size={20} strokeWidth={1.75} />
                  </span>
                  <HealthDot health={h} />
                </div>
                <strong>{tile.name}</strong>
                <small>{tile.purpose}</small>
                <span className="tile-status">
                  <StatusPill value={h} />
                </span>
              </a>
            );
          })}
        </div>
      </Section>

      <Section title="Build" description="Code, CI and workspaces — for makers. Pocket ID gates each app.">
        <div className="tile-grid">
          {build.map((tile) => {
            const h = healthById.get(tile.id) ?? "unknown";
            const Icon = tile.icon;
            return (
              <a key={tile.id} className="tile" href={tileHref(tile)} target="_blank" rel="noreferrer">
                <div className="tile-head">
                  <span className="tile-icon" aria-hidden="true">
                    <Icon size={20} strokeWidth={1.75} />
                  </span>
                  <HealthDot health={h} />
                </div>
                <strong>{tile.name}</strong>
                <small>{tile.purpose}</small>
                <span className="tile-status">
                  <StatusPill value={h} />
                </span>
              </a>
            );
          })}
        </div>
      </Section>


      <p className="muted" style={{ marginTop: 16, fontSize: "0.875rem" }}>
        Health dots are live and refresh every 15 seconds. Access is controlled by Pocket ID — your tailnet membership is the gate.
      </p>

      <style>{`
        .tile-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px,1fr)); gap: 12px; }
        .tile { display: grid; gap: 6px; padding: 16px; border: 1px solid var(--line, #e5e7eb); border-radius: 12px; background: var(--surface, #fff); text-decoration: none; color: inherit; transition: border-color 150ms ease, transform 150ms ease; }
        .tile:hover { border-color: var(--ink-muted, #9ca3af); transform: translateY(-1px); }
        .tile-head { display:flex; justify-content: space-between; align-items: center; }
        .tile-icon { font-size: 1.25rem; line-height: 1; }
        .tile-status { margin-top: 4px; }
        @media (prefers-reduced-motion: reduce) { .tile { transition: none; } }
      `}</style>
    </div>
  );
}

export default HomePage;

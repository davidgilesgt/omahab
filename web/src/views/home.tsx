import { useQuery } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { ErrorState, LoadingState, Section, StatusPill } from "../components/ui";
import { AppIcon } from "../components/appIcon";

type TileDef = {
  id: string;
  name: string;
  purpose: string;
  route: string;
  group: "everyday" | "build";
};

const TILES: TileDef[] = [
  { id: "immich", name: "Photos", purpose: "Private photo library", route: "photos", group: "everyday" },
  { id: "paperless-ngx", name: "Archive", purpose: "Document archive", route: "archive", group: "everyday" },
  { id: "karakeep", name: "Keep", purpose: "Bookmarks & saves", route: "keep", group: "everyday" },
  { id: "syncthing", name: "Sync", purpose: "File sync", route: "sync", group: "everyday" },
  { id: "hermes", name: "AI", purpose: "Your AI assistant", route: "ai", group: "everyday" },
  { id: "forgejo", name: "Git", purpose: "Code hosting", route: "git", group: "build" },
  { id: "woodpecker", name: "CI", purpose: "Builds & deploys", route: "ci", group: "build" },
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
  const launchById = new Map<string, string>();
  if (query.data?.apps) {
    for (const app of query.data.apps) {
      healthById.set(app.id, app.health);
      if (app.launch_url) launchById.set(app.id, app.launch_url);
    }
  }

  const everyday = TILES.filter((t) => t.group === "everyday");
  const build = TILES.filter((t) => t.group === "build");

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1>Welcome home</h1>
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

      <Section title="Everyday">
        <div className="tile-grid">
          {everyday.map((tile) => {
            const h = healthById.get(tile.id) ?? "unknown";
            const href = launchById.get(tile.id) ?? tileHref(tile);
            return (
              <a key={tile.id} className="tile" href={href} target="_blank" rel="noreferrer">
                <div className="tile-head">
                  <span className="tile-icon" aria-hidden="true">
                    <AppIcon bundleId={tile.id} size={20} />
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

      <Section title="Build">
        <div className="tile-grid">
          {build.map((tile) => {
            const h = healthById.get(tile.id) ?? "unknown";
            const href = launchById.get(tile.id) ?? tileHref(tile);
            return (
              <a key={tile.id} className="tile" href={href} target="_blank" rel="noreferrer">
                <div className="tile-head">
                  <span className="tile-icon" aria-hidden="true">
                    <AppIcon bundleId={tile.id} size={20} />
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

    </div>
  );
}

export default HomePage;

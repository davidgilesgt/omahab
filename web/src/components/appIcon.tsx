import { Bell, Bookmark, Cpu, FileText, FolderSync, GitBranch, Hammer, Image, KeyRound, Server, Sparkles } from "lucide-react";
import type { LucideIcon } from "lucide-react";

const ICONS: Record<string, LucideIcon> = {
  "pocket-id": KeyRound,
  forgejo: GitBranch,
  woodpecker: Hammer,
  immich: Image,
  "paperless-ngx": FileText,
  karakeep: Bookmark,
  syncthing: FolderSync,
  litellm: Cpu,
  ntfy: Bell,
  hermes: Sparkles,
};

// Decorative bundle icon; adjacent app names supply the accessible label.
export function AppIcon({ bundleId, size = 18 }: { bundleId: string; size?: number }) {
  const Icon = ICONS[bundleId] ?? Server;
  return <Icon size={size} strokeWidth={1.75} aria-hidden="true" />;
}

export default AppIcon;

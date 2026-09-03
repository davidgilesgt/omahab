import { useEffect, useRef } from "react";
import { QueryClient, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { Application, Backup, ControlEvent, Workspace } from "../api/types";

function routeEvent(type: string): string[] {
  if (type.startsWith("backup.")) return ["backups"];
  if (type.startsWith("service.") || type.startsWith("health.") || type.startsWith("host.") || type === "applications.catalog_missing") return ["applications"];
  if (type.startsWith("deployment.") || type.startsWith("ci.")) return ["projects"];
  if (type.startsWith("syncthing.")) return ["sync-folders"];
  if (type.includes("workspace")) return ["workspaces"];
  if (type.includes("application")) return ["applications"];
  return [];
}

function tryPatchCache(ev: ControlEvent, queryClient: QueryClient): boolean {
  const rid = ev.resource_id ? String(ev.resource_id) : "";
  const data = (ev.data ?? {}) as Record<string, unknown>;
  // service.* / health.* -> applications (and status)
  if ((ev.type.startsWith("service.") || ev.type.startsWith("health.")) && rid) {
    const prev = queryClient.getQueryData<Application[]>(["applications"]);
    if (Array.isArray(prev)) {
      let patched = false;
      const next = prev.map((item) => {
        if (String(item.id) === rid) {
          patched = true;
          if (data && typeof data === "object" && Object.keys(data).length > 0) {
            return { ...item, ...data, id: item.id } as Application;
          }
          const healthVal = data["health"];
          const statusVal = data["status"];
          const health = typeof healthVal === "string" ? healthVal : typeof statusVal === "string" ? statusVal : "";
          if (health) return { ...item, health: health as Application["health"] } as Application;
        }
        return item;
      });
      if (patched) {
        queryClient.setQueryData<Application[]>(["applications"], next);
        const healthVal = data["health"];
        const statusVal = data["status"];
        const h = typeof healthVal === "string" ? healthVal : typeof statusVal === "string" ? statusVal : "";
        if (h) {
          queryClient.setQueryData(["status"], (old: unknown) => {
            if (old && typeof old === "object") {
              return { ...(old as Record<string, unknown>), health: h };
            }
            return old;
          });
        }
        return true;
      }
      if (data && typeof data === "object" && "id" in data) {
        const did = data["id"];
        if ((typeof did === "string" || typeof did === "number") && String(did) === rid) {
          queryClient.setQueryData<Application[]>(["applications"], [...prev, data as unknown as Application]);
          return true;
        }
      }
    }
    return false;
  }
  if (ev.type.startsWith("backup.") && rid) {
    const prev = queryClient.getQueryData<Backup[]>(["backups"]);
    if (Array.isArray(prev)) {
      let patched = false;
      const next = prev.map((b) => {
        if (String(b.id) === rid) {
          patched = true;
          if (data && typeof data === "object" && Object.keys(data).length > 0) return { ...b, ...data } as Backup;
        }
        return b;
      });
      if (patched) {
        queryClient.setQueryData<Backup[]>(["backups"], next);
        return true;
      }
      if (data && typeof data === "object" && "id" in data) {
        const did = data["id"];
        if ((typeof did === "string" || typeof did === "number") && String(did) === rid) {
          queryClient.setQueryData<Backup[]>(["backups"], [...prev, data as unknown as Backup]);
          return true;
        }
      }
    }
    return false;
  }
  if (ev.type.includes("workspace") && rid) {
    const prev = queryClient.getQueryData<Workspace[]>(["workspaces"]);
    if (Array.isArray(prev)) {
      if (ev.type === "workspace.deleted") {
        const filtered = prev.filter((ws) => String(ws.id) !== rid);
        if (filtered.length !== prev.length) {
          queryClient.setQueryData<Workspace[]>(["workspaces"], filtered);
          return true;
        }
      }
      let patched = false;
      const next = prev.map((ws) => {
        if (String(ws.id) === rid) {
          patched = true;
          if (data && typeof data === "object" && Object.keys(data).length > 0) return { ...ws, ...data } as Workspace;
        }
        return ws;
      });
      if (patched) {
        queryClient.setQueryData<Workspace[]>(["workspaces"], next);
        return true;
      }
      if (data && typeof data === "object" && "id" in data) {
        const did = data["id"];
        if ((typeof did === "string" || typeof did === "number") && String(did) === rid) {
          queryClient.setQueryData<Workspace[]>(["workspaces"], [...prev, data as unknown as Workspace]);
          return true;
        }
      }
    }
    return false;
  }
  return false;
}


export function useEventStream() {
  const { client, token } = useAuth();
  const queryClient = useQueryClient();
  const lastIdRef = useRef<string | null>(null);

  useEffect(() => {
    if (!token) return;

    const cached = queryClient.getQueryData<ControlEvent[]>(["events"]);
    if (cached && cached.length > 0) lastIdRef.current = cached[0]?.id ?? null;

    const controller = new AbortController();
    let stopped = false;
    let retryDelay = 1000;
    let retryTimer: number | undefined;

    async function run() {
      while (!stopped && !controller.signal.aborted) {
        try {
          await client.streamEvents(
            controller.signal,
            (ev) => {
              lastIdRef.current = ev.id;
              retryDelay = 1000;
              queryClient.setQueryData<ControlEvent[]>(["events"], (old) => {
                if (!old) return [ev];
                if (old.some((item) => item.id === ev.id)) return old;
                return [ev, ...old];
              });
              if (!tryPatchCache(ev, queryClient)) {
                const keys = routeEvent(ev.type);
                for (const key of keys) queryClient.invalidateQueries({ queryKey: [key] });
                if (ev.type.startsWith("service.") || ev.type.startsWith("health.")) queryClient.invalidateQueries({ queryKey: ["status"] });
              }
            },
            lastIdRef.current ?? undefined,
          );
          if (stopped || controller.signal.aborted) break;
        } catch {
          if (stopped || controller.signal.aborted) break;
        }
        if (stopped || controller.signal.aborted) break;
        await new Promise<void>((resolve) => {
          retryTimer = window.setTimeout(resolve, retryDelay);
        });
        retryDelay = Math.min(retryDelay * 2, 30_000);
      }
    }

    void run();

    return () => {
      stopped = true;
      controller.abort();
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
    };
  }, [client, token, queryClient]);
}

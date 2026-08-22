import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { ControlEvent } from "../api/types";

function routeEvent(type: string): string[] {
  if (type.startsWith("backup.")) return ["backups"];
  if (type.startsWith("service.") || type.startsWith("health.") || type.startsWith("host.") || type === "applications.catalog_missing") return ["applications"];
  if (type.startsWith("deployment.") || type.startsWith("ci.")) return ["projects"];
  if (type.startsWith("syncthing.")) return ["sync-folders"];
  if (type.includes("workspace")) return ["workspaces"];
  if (type.includes("application")) return ["applications"];
  return [];
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
              const keys = routeEvent(ev.type);
              for (const key of keys) queryClient.invalidateQueries({ queryKey: [key] });
              if (ev.type.startsWith("service.") || ev.type.startsWith("health.")) queryClient.invalidateQueries({ queryKey: ["status"] });
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

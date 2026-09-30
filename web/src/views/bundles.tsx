import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import type { BundleAdmin } from "../api/types";
import { EmptyState, ErrorState, humanizeStatus, LoadingState, PageHeader, Section, StatusPill } from "../components/ui";
import { DestructiveConfirm } from "../components/destructiveConfirm";
import { useToast } from "../components/toast";

/** Bundle management: merged curated+custom list, add/remove/enable/disable,
 *  and apply (rebuild + switch) with operation polling. */
export function BundlesPage() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [applyOpId, setApplyOpId] = useState<string | null>(null);
  const [confirmRemove, setConfirmRemove] = useState<BundleAdmin | null>(null);

  const bundles = useQuery({
    queryKey: ["bundles"],
    queryFn: client.bundles,
  });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["bundles"] });
    void queryClient.invalidateQueries({ queryKey: ["catalog"] });
  };

  const apply = useQuery({
    queryKey: ["bundles", "apply", applyOpId],
    queryFn: () => client.operation(applyOpId as string),
    enabled: applyOpId !== null,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status === "running" || status === "pending" ? 5_000 : false;
    },
  });

  const applyMutation = useMutation({
    mutationFn: () => client.applyBundles(),
    onSuccess: (op) => {
      setApplyOpId(op.id);
      toast.success("Bundle apply started");
      invalidate();
    },
    onError: (error) => toast.error(error.message),
  });

  const disableMutation = useMutation({
    mutationFn: (input: { id: string; disabled: boolean }) => client.setBundleDisabled(input.id, input.disabled),
    onSuccess: (_data, input) => {
      toast.success(input.disabled ? "Bundle disabled" : "Bundle enabled");
      invalidate();
    },
    onError: (error) => toast.error(error.message),
  });

  const removeMutation = useMutation({
    mutationFn: (id: string) => client.removeBundle(id),
    onSuccess: () => {
      toast.success("Bundle removed. Apply to activate the removal.");
      setConfirmRemove(null);
      invalidate();
    },
    onError: (error) => toast.error(error.message),
  });

  const opDone = apply.data && apply.data.status !== "running" && apply.data.status !== "pending";

  return (
    <div className="page">
      <PageHeader
        title="Bundles"
        description="Curated bundles ship with omahab; custom bundles live in the machine's bundles repo and apply at the next rebuild."
      />

      <Section title="Apply">
        <p>
          Applying rebuilds the system with the current bundles repo (add/remove/enable/disable are staged
          until then) and switches. The panel restarts during the switch; this page resumes the operation.
        </p>
        <div className="button-row">
          <button className="button" disabled={applyMutation.isPending} onClick={() => applyMutation.mutate()}>
            {applyMutation.isPending ? "Starting…" : "Apply bundles"}
          </button>
        </div>
        {applyOpId && apply.data && (
          <p role="status">
            Operation <code className="mono">{applyOpId}</code>:{" "}
            <StatusPill value={apply.data.status} /> {opDone && apply.data.error ? <span>{apply.data.error}</span> : null}
            {opDone ? (
              <span className="hint"> — new bundles are live in the catalog.</span>
            ) : (
              <span className="hint"> — running; the panel restarts during the switch.</span>
            )}
          </p>
        )}
      </Section>

      <AddBundleSection onAdded={invalidate} />

      <Section title="Bundles">
        {bundles.isLoading ? (
          <LoadingState label="Loading bundles" />
        ) : bundles.isError ? (
          <ErrorState error={bundles.error} retry={() => void bundles.refetch()} />
        ) : !bundles.data || bundles.data.length === 0 ? (
          <EmptyState title="No bundles" description="The catalog is empty and no custom bundles exist yet." />
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Bundle</th>
                <th>Source</th>
                <th>Port</th>
                <th>State</th>
                <th>Last apply</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {bundles.data.map((bundle) => (
                <tr key={bundle.id}>
                  <td>
                    <strong>{bundle.name || bundle.id}</strong>
                    <small className="mono"> {bundle.id}</small>
                  </td>
                  <td>
                    <StatusPill value={bundle.source === "custom" ? "custom" : "catalog"} />
                  </td>
                  <td>{bundle.port ? <code className="mono">{bundle.port}</code> : "—"}</td>
                  <td>
                    {bundle.disabled ? <StatusPill value="disabled" /> : <StatusPill value="enabled" />}
                    {bundle.dirty ? <small className="hint"> · uncommitted changes</small> : null}
                  </td>
                  <td>{bundle.last_apply ? humanizeStatus(bundle.last_apply) : "—"}</td>
                  <td>
                    {bundle.source === "custom" ? (
                      <div className="button-row">
                        <button
                          className="button secondary"
                          disabled={disableMutation.isPending}
                          onClick={() => disableMutation.mutate({ id: bundle.id, disabled: !bundle.disabled })}
                        >
                          {bundle.disabled ? "Enable" : "Disable"}
                        </button>
                        <button className="button danger" onClick={() => setConfirmRemove(bundle)}>
                          Remove
                        </button>
                      </div>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Section>

      {confirmRemove ? (
        <DestructiveConfirm
          title={`Remove custom bundle ${confirmRemove.id}?`}
          description="The bundle is removed from the repo and disappears from the system at the next apply."
          confirmValue={confirmRemove.id}
          confirmLabel="Remove"
          pending={removeMutation.isPending}
          onConfirm={() => removeMutation.mutate(confirmRemove.id)}
          onClose={() => setConfirmRemove(null)}
        />
      ) : null}
    </div>
  );
}

function AddBundleSection({ onAdded }: { onAdded: () => void }) {
  const { client } = useAuth();
  const toast = useToast();
  const [bundleJSON, setBundleJSON] = useState("");
  const [moduleNix, setModuleNix] = useState("");

  const create = useMutation({
    mutationFn: () =>
      client.createBundle({
        bundle_json: bundleJSON,
        module_nix: moduleNix,
      }),
    onSuccess: (bundle) => {
      toast.success(`Bundle ${bundle.id} added. Apply to activate it.`);
      setBundleJSON("");
      setModuleNix("");
      onAdded();
    },
    onError: (error) => toast.error(error.message),
  });

  return (
    <Section title="Add custom bundle">
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
          create.mutate();
        }}
      >
        <label className="field">
          <span>bundle.json</span>
          <textarea
            rows={8}
            className="mono"
            placeholder={'{\n  "id": "myapp",\n  "name": "My App",\n  "units": ["myapp.service"],\n  "port": 20500,\n  "health_check": { "kind": "http", "port": 20500, "path": "/" },\n  "resources": { "memory_mb": 256 }\n}'}
            value={bundleJSON}
            onChange={(event) => setBundleJSON(event.target.value)}
          />
          <small className="hint">Port must be in the reserved custom range 20000–20999.</small>
        </label>
        <label className="field">
          <span>module.nix</span>
          <textarea
            rows={8}
            className="mono"
            placeholder={"{ config, pkgs, ... }: {\n  # systemd / package config for the bundle\n}"}
            value={moduleNix}
            onChange={(event) => setModuleNix(event.target.value)}
          />
          <small className="hint">Nix module text. Secrets never belong here — they flow through env at deploy time.</small>
        </label>
        <div className="button-row">
          <button type="submit" className="button" disabled={create.isPending || !bundleJSON.trim() || !moduleNix.trim()}>
            {create.isPending ? "Adding…" : "Add bundle"}
          </button>
        </div>
      </form>
    </Section>
  );
}

import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useToast } from "../components/toast";
import { ErrorState, LoadingState } from "../components/ui";
import type { IndexSetupOption, ModelInfo } from "../api/types";

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "—";
  const units = ["B", "KB", "MB", "GB"];
  let size = value;
  let unitIndex = 0;
  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024;
    unitIndex += 1;
  }
  const formatted = unitIndex === 0 ? `${Math.round(size)}` : size >= 10 ? `${Math.round(size)}` : `${size.toFixed(1)}`;
  return `${formatted} ${units[unitIndex]}`;
}

export function IndexSetupControl({ onSaved }: { onSaved?: () => void }) {
  const { client } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const optionsQuery = useQuery({ queryKey: ["knowledge", "index-setup-options"], queryFn: client.knowledgeIndexSetupOptions });
  const modelsQuery = useQuery({ queryKey: ["knowledge", "pinned-models"], queryFn: client.knowledgePinnedModels });
  const choiceQuery = useQuery({ queryKey: ["knowledge", "index-setup"], queryFn: client.knowledgeGetIndexSetup });

  const rawChoice = choiceQuery.data?.choice ?? "";
  // Mirror the server normalization (setup_status.go): trim, lowercase,
  // legacy "fulltext" reads as "full_text". Raw SQL rows may carry any
  // casing/whitespace the validated setter would never write.
  const normalized = rawChoice.trim().toLowerCase();
  const persisted = normalized === "fulltext" ? "full_text" : normalized;

  // Draft selection with an explicit dirty flag. The draft initializes when
  // the persisted choice first arrives and re-syncs to later persisted
  // changes only while clean, so a background refetch never clobbers an
  // unsaved selection. The dirty flag resets only on successful save.
  const [draft, setDraft] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);

  useEffect(() => {
    if (!choiceQuery.isSuccess || dirty) return;
    if (draft !== persisted) setDraft(persisted);
  }, [choiceQuery.isSuccess, persisted, dirty, draft]);

  const mutation = useMutation({
    mutationFn: (choice: string) => client.knowledgeSetIndexSetup(choice),
    onSuccess: (_data, choice) => {
      setDirty(false);
      onSaved?.();
      void queryClient.invalidateQueries({ queryKey: ["knowledge", "index-setup"] });
      void queryClient.invalidateQueries({ queryKey: ["setup"] });
      const opts = optionsQuery.data ?? [];
      const label = opts.find((item) => item.id === choice || (choice === "fulltext" && item.id === "full_text"))?.label ?? choice;
      toast.success(`Index setup saved: ${label}`);
    },
    onError: (err: unknown) => toast.error(err instanceof Error ? err.message : "Could not save choice"),
  });

  // Pinned models are display metadata only: their failure must not take
  // down the control. Options + choice render regardless; a models failure
  // surfaces as an inline nonblocking notice below.
  if (optionsQuery.isLoading || choiceQuery.isLoading) return <LoadingState label="Loading index options" />;
  if (optionsQuery.isError) return <ErrorState error={optionsQuery.error} retry={() => void optionsQuery.refetch()} />;
  if (choiceQuery.isError) return <ErrorState error={choiceQuery.error} retry={() => void choiceQuery.refetch()} />;

  const options = (optionsQuery.data ?? []) as IndexSetupOption[];
  const models = (modelsQuery.data ?? []) as ModelInfo[];

  if (!options.length) {
    return <p className="muted">No index options are available from the server.</p>;
  }

  // Never infer a choice: an empty persisted value (or an uninitialized
  // draft) renders the disabled placeholder, not a preselected model.
  const effective = draft ?? persisted;
  const selected = options.find((option) => option.id === effective) ?? null;
  const saving = mutation.isPending;
  const canSave = dirty && effective !== "" && effective !== persisted && !saving;

  function handleChange(value: string) {
    setDraft(value);
    setDirty(value !== persisted);
  }

  function handleSave() {
    if (!canSave) return;
    mutation.mutate(effective);
  }

  const alias = selected?.model_alias ?? null;
  const model = alias ? models.find((entry) => entry.alias === alias) ?? null : null;

  return (
    <div className="form-stack">
      {modelsQuery.isError ? (
        <p className="inline-error" role="alert">
          Model details unavailable{modelsQuery.error instanceof Error ? `: ${modelsQuery.error.message}` : ""}{" "}
          <button type="button" className="button secondary" onClick={() => void modelsQuery.refetch()}>
            Retry
          </button>
        </p>
      ) : null}
      <label className="field">
        <span>Document search</span>
        <select aria-label="Document search" value={effective} onChange={(event) => handleChange(event.currentTarget.value)} disabled={saving}>
          <option value="" disabled>
            Choose indexing…
          </option>
          {options.map((option) => (
            <option key={option.id} value={option.id}>
              {option.label}
            </option>
          ))}
        </select>
      </label>
      {modelsQuery.isLoading && selected && alias ? <p className="muted">Loading model details…</p> : null}
      {selected && !alias ? (
        <p className="muted">No download · No additional memory · Text matching only</p>
      ) : null}
      {selected?.description ? <p className="muted">{selected.description}</p> : null}
      {selected && alias && model ? (
        <p className="muted">
          Model <span className="mono">{model.name}</span> · License <strong>{model.license}</strong> · Download {formatBytes(model.size_bytes)} · Memory {model.expected_memory_mb} MB
        </p>
      ) : null}
      {selected && alias && !model && !modelsQuery.isLoading ? (
        <p className="muted">Model {alias} — metadata not available yet</p>
      ) : null}
      <div className="row-actions">
        <button className="button primary" type="button" onClick={handleSave} disabled={!canSave}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
      <small>The embedding worker uses the selected model for new indexes.</small>
      {mutation.isError ? (
        <p className="inline-error" role="alert">{mutation.error instanceof Error ? mutation.error.message : "Could not save choice"}</p>
      ) : null}
    </div>
  );
}

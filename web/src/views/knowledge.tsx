import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useToast } from "../components/toast";
import { ErrorState, LoadingState, Section } from "../components/ui";
import type { IndexSetupOption, ModelInfo } from "../api/types";

const INDEX_SETUP_KEY = "omahab.knowledge.indexSetup";
const PRINCIPAL = "default";

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

function ConsentDialog({
  provider,
  pending,
  error,
  onGrant,
  onDecline,
  onClose,
}: {
  provider: string;
  pending: boolean;
  error: unknown;
  onGrant: () => void;
  onDecline: () => void;
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = dialogRef.current;
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      if (dialog?.open) dialog.close();
      previous?.focus();
    };
  }, []);
  return (
    <dialog
      ref={dialogRef}
      className="modal"
      aria-labelledby="consent-title"
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
    >
      <header>
        <div>
          <p className="eyebrow">Remote summarization</p>
          <h2 id="consent-title">Allow summarization with {provider}?</h2>
        </div>
        <button type="button" className="icon-button" onClick={onClose} aria-label="Close">
          ×
        </button>
      </header>
      <div className="form-stack">
        <p>
          The assistant will send the selected document text to <strong>{provider}</strong> for remote summarization. Private content leaves
          this server and is processed by the provider. Choose explicitly.
        </p>
        <div className="inline-warning" role="note">
          <strong>Informed choice required.</strong> Grant access only if you trust {provider} with private excerpts. You can change this
          later by declining.
        </div>
        {error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "The consent could not be saved."}</p> : null}
        <div className="modal-actions">
          <button type="button" className="button ghost" onClick={onDecline} disabled={pending}>
            Decline
          </button>
          <button type="button" className="button primary" onClick={onGrant} disabled={pending}>
            {pending ? "Saving…" : `Grant for ${provider}`}
          </button>
        </div>
        <small>
          Consent is stored per provider and can be withdrawn by declining again. Current choice is saved for this principal.
        </small>
      </div>
    </dialog>
  );
}

function IndexSetupOptionsBlock() {
  const { client } = useAuth();
  const toast = useToast();
  const optionsQuery = useQuery({ queryKey: ["knowledge", "index-setup-options"], queryFn: client.knowledgeIndexSetupOptions });
  const modelsQuery = useQuery({ queryKey: ["knowledge", "pinned-models"], queryFn: client.knowledgePinnedModels });
  const [selected, setSelected] = useState<string | null>(() => {
    try {
      return localStorage.getItem(INDEX_SETUP_KEY);
    } catch {
      return null;
    }
  });

  if (optionsQuery.isLoading || modelsQuery.isLoading) return <LoadingState label="Loading index options" />;
  if (optionsQuery.isError) return <ErrorState error={optionsQuery.error} retry={() => void optionsQuery.refetch()} />;
  if (modelsQuery.isError) return <ErrorState error={modelsQuery.error} retry={() => void modelsQuery.refetch()} />;

  const options = (optionsQuery.data ?? []) as IndexSetupOption[];
  const models = (modelsQuery.data ?? []) as ModelInfo[];

  // The service returns exactly three options; render whatever is returned without adding or removing entries.
  if (!options.length) {
    return <p className="muted">No index options are available from the server.</p>;
  }

  function handleSelect(id: string) {
    setSelected(id);
    try {
      localStorage.setItem(INDEX_SETUP_KEY, id);
    } catch {
      // Storage may be unavailable in private mode; selection remains in memory.
    }
    const label = options.find((item) => item.id === id)?.label ?? id;
    toast.success(`Index preference set to ${label}`);
  }

  return (
    <div className="form-stack">
      <div className="resource-list inset" role="radiogroup" aria-label="Semantic index setup">
        {options.map((option) => {
          const alias = option.model_alias ?? null;
          const model = alias ? models.find((entry) => entry.alias === alias) ?? null : null;
          const isSelected = selected === option.id;
          return (
            <label
              key={option.id}
              className="resource-row"
              style={{ cursor: "pointer", alignItems: "flex-start", gap: "0.75rem" }}
            >
              <input
                type="radio"
                name="knowledge-index-setup"
                value={option.id}
                checked={isSelected}
                onChange={() => handleSelect(option.id)}
                style={{ marginTop: "0.4rem", width: "1rem", minHeight: "1rem" }}
                aria-label={option.label}
              />
              <span style={{ minWidth: 0, flex: 1, display: "grid", gap: "0.35rem" }}>
                <span style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: "0.5rem" }}>
                  <strong>{option.label}</strong>
                  {isSelected ? <span className="status status-positive" style={{ padding: "0.15rem 0.4rem" }}>Selected</span> : null}
                </span>
                <small style={{ color: "var(--ink-muted)" }}>{option.description}</small>
                {alias ? (
                  model ? (
                    <span
                      style={{
                        display: "grid",
                        gap: "0.2rem",
                        marginTop: "0.35rem",
                        padding: "0.6rem 0.75rem",
                        border: "var(--border) solid var(--line)",
                        borderRadius: "var(--radius-sm)",
                        background: "var(--surface-raised)",
                        fontSize: "0.875rem",
                      }}
                    >
                      <span>
                        Model <span className="mono">{model.name}</span>
                        {model.model_id ? <span className="muted"> · {model.model_id}</span> : null}
                      </span>
                      <span>
                        License <strong>{model.license}</strong> · Download {formatBytes(model.size_bytes)} · Memory {model.expected_memory_mb} MB
                      </span>
                    </span>
                  ) : (
                    <span className="muted" style={{ fontSize: "0.875rem" }}>
                      Model {alias} — metadata not available yet
                    </span>
                  )
                ) : (
                  <span className="muted" style={{ fontSize: "0.875rem" }}>
                    No download · No additional memory · Text matching only
                  </span>
                )}
              </span>
            </label>
          );
        })}
      </div>
      <small>
        No write endpoint exists for this choice. The selection is stored locally in this browser and does not change server state.
      </small>
    </div>
  );
}

function SummarizationConsentBlock() {
  const { client } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [provider, setProvider] = useState("openai");
  const trimmed = provider.trim();
  const normalizedProvider = trimmed.length ? trimmed : "openai";
  const consentQuery = useQuery({
    queryKey: ["knowledge", "consent", PRINCIPAL, normalizedProvider],
    queryFn: () => client.knowledgeGetConsent(normalizedProvider, PRINCIPAL),
    enabled: trimmed.length > 0,
  });
  const [dialogOpen, setDialogOpen] = useState(false);

  const setConsent = useMutation({
    mutationFn: (granted: boolean) => client.knowledgeSetConsent(normalizedProvider, granted, PRINCIPAL),
    onSuccess: (_data, granted) => {
      void queryClient.invalidateQueries({ queryKey: ["knowledge", "consent", PRINCIPAL, normalizedProvider] });
      if (granted) {
        toast.success(`Consent granted for ${normalizedProvider}`);
      } else {
        toast.success(`Consent declined for ${normalizedProvider}`);
      }
      setDialogOpen(false);
    },
    onError: (error: unknown) => {
      const message = error instanceof Error ? error.message : "Could not save consent";
      toast.error(message);
    },
  });

  function requestSummarize() {
    if (!trimmed) {
      toast.error("Enter a provider name");
      return;
    }
    if (consentQuery.data?.granted) {
      toast.success(`Summarization with ${normalizedProvider} — consent already granted`);
      return;
    }
    setDialogOpen(true);
  }

  const granted = consentQuery.data?.granted ?? false;
  const isChecking = consentQuery.isLoading;

  return (
    <div className="form-stack">
      <div className="form-inline" style={{ alignItems: "end" }}>
        <label>
          Provider
          <input
            value={provider}
            onChange={(event) => setProvider(event.currentTarget.value)}
            placeholder="openai"
            spellCheck={false}
            autoComplete="off"
          />
        </label>
        <div className="row-actions" style={{ justifyContent: "flex-start" }}>
          <button type="button" className="button primary" onClick={requestSummarize} disabled={!trimmed || setConsent.isPending}>
            Summarize with {normalizedProvider}
          </button>
        </div>
        <span className="muted" style={{ fontSize: "0.875rem", minWidth: "10rem" }}>
          {isChecking ? "Checking consent…" : consentQuery.isError ? "Consent check failed" : granted ? "Consent granted" : "Consent not granted"}
        </span>
      </div>
      {consentQuery.isError ? (
        <p className="inline-error" role="alert">
          {consentQuery.error instanceof Error ? consentQuery.error.message : "Could not check consent"}
        </p>
      ) : null}
      <small>
        Remote summarization sends private document text to the named provider. An explicit grant or decline is required and is
        persisted per provider.
      </small>
      {dialogOpen ? (
        <ConsentDialog
          provider={normalizedProvider}
          pending={setConsent.isPending}
          error={setConsent.error}
          onGrant={() => setConsent.mutate(true)}
          onDecline={() => setConsent.mutate(false)}
          onClose={() => setDialogOpen(false)}
        />
      ) : null}
    </div>
  );
}

export function AssistantKnowledgePanel() {
  return (
    <div style={{ display: "grid", gap: "1.5rem" }}>
      <Section
        title="Assistant knowledge"
        description="Choose how private sources are indexed for semantic search. Model details come from the server’s pinned models."
      >
        <IndexSetupOptionsBlock />
      </Section>
      <Section
        title="Remote summarization"
        description="Grant or decline per-provider consent before any document text is sent for remote summarization."
      >
        <SummarizationConsentBlock />
      </Section>
    </div>
  );
}

export default AssistantKnowledgePanel;
